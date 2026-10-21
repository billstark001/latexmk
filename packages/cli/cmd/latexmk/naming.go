package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/exporthistory"
	"github.com/billstark001/latexmk/packages/cli/internal/naming"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// Invocation state survives target iteration, realtime reconnects and settings
// reloads. No process-global clock or mutable Git/template state is needed.
type nameInvocation struct {
	at      time.Time
	now     func() time.Time
	git     map[string]string
	outputs map[string]*generatedPaths
}

func newNameInvocation() *nameInvocation {
	return &nameInvocation{at: time.Now(), now: time.Now, git: make(map[string]string),
		outputs: make(map[string]*generatedPaths)}
}

type generatedPaths struct {
	mu           sync.RWMutex
	paths        map[string]struct{}
	reservations map[string]int
}

type nameReservation struct {
	once  sync.Once
	paths *generatedPaths
	name  string
}

func (g *generatedPaths) reserve(name string) *nameReservation {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reservations == nil {
		g.reservations = make(map[string]int)
	}
	g.reservations[name]++
	return &nameReservation{paths: g, name: name}
}

func (r *nameReservation) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.paths.mu.Lock()
		defer r.paths.mu.Unlock()
		r.paths.reservations[r.name]--
		if r.paths.reservations[r.name] == 0 {
			delete(r.paths.reservations, r.name)
		}
	})
}

func releaseBuildNaming(opts compileOptions) { opts.exportLease.Close() }

func (g *generatedPaths) refresh(projectRoot string) error {
	paths, err := exporthistory.Paths(projectRoot)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths = make(map[string]struct{}, len(paths))
	for _, name := range paths {
		g.paths[name] = struct{}{}
	}
	return nil
}

func (g *generatedPaths) add(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths[name] = struct{}{}
}

func (g *generatedPaths) snapshot() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	paths := make([]string, 0, len(g.paths)+len(g.reservations))
	for name := range g.paths {
		paths = append(paths, name)
	}
	for name := range g.reservations {
		if _, exists := g.paths[name]; !exists {
			paths = append(paths, name)
		}
	}
	slices.Sort(paths)
	return paths
}

func generatedFiles(opts compileOptions) func() ([]string, error) {
	if opts.generated == nil {
		return nil
	}
	return func() ([]string, error) {
		if err := opts.generated.refresh(opts.projectRoot); err != nil {
			return nil, err
		}
		return opts.generated.snapshot(), nil
	}
}

func prepareGeneratedPaths(opts *compileOptions) error {
	if opts.invocation == nil {
		opts.invocation = newNameInvocation()
	}
	if existing := opts.invocation.outputs[opts.projectRoot]; existing != nil {
		opts.generated = existing
		return existing.refresh(opts.projectRoot)
	}
	paths, err := exporthistory.Paths(opts.projectRoot)
	if err != nil {
		return err
	}
	g := &generatedPaths{paths: make(map[string]struct{}, len(paths))}
	for _, name := range paths {
		g.add(name)
	}
	opts.generated = g
	opts.invocation.outputs[opts.projectRoot] = g
	return nil
}

type pdfNamingPlan struct {
	template     *naming.Template
	initial      string
	previewLease *nameReservation
}

func retentionCount(retention *config.PDFRetention) *int {
	if retention == nil {
		return nil
	}
	return retention.KeepLast
}

func templateValues(ctx context.Context, t *naming.Template, opts compileOptions) (naming.Values, error) {
	if opts.invocation == nil {
		return nil, errors.New("naming invocation is not initialized")
	}
	at := opts.invocation.at
	if opts.nameScope == "build" {
		at = opts.invocation.now()
	}
	location := time.Local
	if opts.nameTimeZone != "" && opts.nameTimeZone != "local" {
		var err error
		location, err = time.LoadLocation(opts.nameTimeZone)
		if err != nil {
			return nil, err
		}
	}
	at = at.In(location)
	stem := strings.TrimSuffix(filepath.Base(opts.entry), filepath.Ext(opts.entry))
	values := naming.Values{
		"entry.stem": naming.String(stem), "project": naming.String(filepath.Base(opts.projectRoot)),
		"engine":   naming.String(opts.engine),
		"date":     naming.Time{Time: at, Layout: time.DateOnly},
		"time":     naming.Time{Time: at, Layout: "15-04-05"},
		"datetime": naming.Time{Time: at, Layout: "20060102T150405"},
	}
	if opts.target != "" {
		values["target"] = naming.String(opts.target)
	}
	for _, name := range t.Variables() {
		if name != "git.commit" {
			if _, ok := values[name]; !ok {
				return nil, fmt.Errorf("template variable %q is unavailable", name)
			}
			continue
		}
		commit := opts.invocation.git[opts.projectRoot]
		if commit == "" || opts.nameScope == "build" {
			operation, cancel := context.WithTimeout(ctx, 2*time.Second)
			command := exec.CommandContext(operation, "git", "-C", opts.projectRoot, "rev-parse", "--verify", "HEAD")
			output := &gitNameOutput{}
			command.Stdout, command.Stderr = output, io.Discard
			err := command.Run()
			cancel()
			if err != nil {
				return nil, fmt.Errorf("read git.commit (Git and a repository with a commit are required): %w", err)
			}
			commit = strings.TrimSpace(output.String())
			if _, err := naming.Hash(commit).Format(""); err != nil {
				return nil, err
			}
			if opts.nameScope != "build" {
				opts.invocation.git[opts.projectRoot] = commit
			}
		}
		values[name] = naming.Hash(commit)
	}
	return values, nil
}

type gitNameOutput struct{ strings.Builder }

func (w *gitNameOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 256 {
		return 0, errors.New("git HEAD output is too large")
	}
	return w.Builder.Write(p)
}

func renderName(ctx context.Context, t *naming.Template, opts compileOptions) (string, error) {
	values, err := templateValues(ctx, t, opts)
	if err != nil {
		return "", err
	}
	return t.Render(values)
}

type filenameValue struct{ naming.Value }

func (v filenameValue) Format(spec string) (string, error) {
	value, err := v.Value.Format(spec)
	if err != nil {
		return "", err
	}
	if err := safefs.ValidateNamePart(value); err != nil {
		return "", err
	}
	return value, nil
}

// Output templates insert filename fragments; directory separators belong in
// literal template text. Plain name.render deliberately keeps arbitrary text.
func renderOutputName(ctx context.Context, t *naming.Template, opts compileOptions) (string, error) {
	values, err := templateValues(ctx, t, opts)
	if err != nil {
		return "", err
	}
	for name, value := range values {
		values[name] = filenameValue{value}
	}
	return t.Render(values)
}

func pdfDestination(root, destination string) (string, error) {
	if filepath.IsAbs(destination) {
		var err error
		destination, err = filepath.Rel(root, destination)
		if err != nil {
			return "", err
		}
	}
	if err := safefs.ValidateOutputPath(destination); err != nil {
		return "", err
	}
	clean, err := safefs.Clean(filepath.ToSlash(destination))
	if err != nil || !strings.EqualFold(filepath.Ext(clean), ".pdf") {
		return "", errors.New("PDF export must be a .pdf path inside the project root")
	}
	if err := safefs.ValidateOutputPath(clean); err != nil {
		return "", err
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = fs.Close() }()
	if err := fs.CheckDestination(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func preparePDFNaming(opts *compileOptions) error {
	if err := prepareGeneratedPaths(opts); err != nil {
		return err
	}
	if opts.pdfTemplate == "" && opts.pdfKeepLast == nil {
		if opts.pdfExport != "" {
			var err error
			opts.pdfExport, err = pdfDestination(opts.projectRoot, opts.pdfExport)
			return err
		}
		return nil
	}
	plan := &pdfNamingPlan{initial: opts.pdfExport}
	if opts.pdfTemplate != "" {
		var err error
		plan.template, err = naming.Parse(opts.pdfTemplate)
		if err != nil {
			return err
		}
		plan.initial, err = renderOutputName(context.Background(), plan.template, *opts)
		if err != nil {
			return err
		}
	}
	if plan.initial == "" {
		return errors.New("PDF retention requires a PDF export destination")
	}
	var err error
	plan.initial, err = pdfDestination(opts.projectRoot, plan.initial)
	if err != nil {
		return err
	}
	opts.naming, opts.pdfExport = plan, plan.initial
	plan.previewLease = opts.generated.reserve(filepath.Join(opts.projectRoot, filepath.FromSlash(plan.initial)))
	return nil
}

func prepareBuildNaming(ctx context.Context, opts compileOptions) (compileOptions, error) {
	if opts.naming == nil {
		return opts, nil
	}
	destination := opts.naming.initial
	if opts.nameScope == "build" && opts.naming.template != nil {
		var err error
		destination, err = renderOutputName(ctx, opts.naming.template, opts)
		if err != nil {
			return opts, err
		}
	}
	var err error
	opts.pdfExport, err = pdfDestination(opts.projectRoot, destination)
	if err == nil {
		opts.exportLease = opts.generated.reserve(filepath.Join(opts.projectRoot, filepath.FromSlash(opts.pdfExport)))
	}
	return opts, err
}

func publishPDF(opts compileOptions, artifact protocol.Artifact) error {
	write := func() error { return client.ExportArtifact(opts.outDir, opts.projectRoot, opts.pdfExport, artifact) }
	if opts.naming == nil {
		return write()
	}
	group := "pdf\x00" + opts.entry + "\x00" + opts.target + "\x00" + opts.engine
	file := exporthistory.File{Path: opts.pdfExport, Size: artifact.Size, SHA256: artifact.SHA256}
	if err := exporthistory.Publish(opts.projectRoot, group, file, opts.pdfKeepLast, write); err != nil {
		return err
	}
	if opts.generated != nil {
		if err := opts.generated.refresh(opts.projectRoot); err != nil {
			return err
		}
	}
	terminalFprintf(os.Stderr, "latexmk: exported PDF: %s\n", filepath.Join(opts.projectRoot, opts.pdfExport))
	return nil
}

func runName(args []string) int {
	jsonOutput := hasJSONFlag(args)
	fail := func(err error) int { return failAgentArguments("name.render", jsonOutput, err) }
	if len(args) == 0 || args[0] != "render" {
		return fail(errors.New("name supports 'render --template TEMPLATE [entry|--target NAME]'"))
	}
	source, common, err := extractTemplateArg(args[1:])
	if err != nil {
		return fail(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, common, err := config.LoadArgs(cwd, common)
	if err != nil {
		return fail(err)
	}
	opts := optionsFromConfig(cfg, false)
	opts.invocation = newNameInvocation()
	if err := parseCompileArgs(common, &opts); err != nil {
		return fail(err)
	}
	if opts.watch || opts.detach || opts.target == "all" || opts.explain != "" {
		return fail(errors.New("name render requires one entry or target"))
	}
	if opts.target == "" && opts.entry == "" {
		opts.target, err = implicitTarget(cfg)
		if err != nil {
			return fail(err)
		}
	}
	if opts.target != "" {
		if err := applyBuildTarget(&opts, cfg, common); err != nil {
			return fail(err)
		}
	}
	if opts.entry == "" {
		return fail(errors.New("no TeX entry file was provided"))
	}
	if err := normalizeCompilePaths(&opts, cwd); err != nil {
		return fail(err)
	}
	template, err := naming.Parse(source)
	if err != nil {
		return fail(err)
	}
	resolved, err := renderName(context.Background(), template, opts)
	if err != nil {
		return fail(err)
	}
	if jsonOutput {
		return writeNameJSON(source, resolved)
	}
	terminalPrintln(resolved)
	return 0
}

func extractTemplateArg(args []string) (string, []string, error) {
	var source string
	var common []string
	found := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			common = append(common, args[i:]...)
			break
		}
		if args[i] != "--template" && !strings.HasPrefix(args[i], "--template=") {
			common = append(common, args[i])
			continue
		}
		if found {
			return "", nil, errors.New("--template can only be supplied once")
		}
		found = true
		_, value, supplied := strings.Cut(args[i], "=")
		if !supplied {
			i++
			if i >= len(args) {
				return "", nil, errors.New("--template requires a value")
			}
			value = args[i]
		}
		source = value
	}
	if !found || source == "" {
		return "", nil, errors.New("--template requires a nonempty value")
	}
	return source, common, nil
}

func writeNameJSON(source, resolved string) int {
	data := struct {
		Template string `json:"template"`
		Resolved string `json:"resolved"`
	}{source, resolved}
	if err := writeAgentJSON("name.render", data); err != nil {
		return fail(err)
	}
	return 0
}
