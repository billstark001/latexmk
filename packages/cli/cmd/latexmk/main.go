package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
	projectwatch "github.com/billstark001/latexmk/packages/cli/internal/watch"
)

var (
	version   = "0.1.0-dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type compileOptions struct {
	server        string
	token         string
	projectRoot   string
	rootMode      string
	uploadMode    string
	manifestFile  string
	includeFiles  []string
	gitIgnore     bool
	engine        string
	outDir        string
	timeout       time.Duration
	interaction   string
	synctex       bool
	haltOnError   bool
	fileLineError bool
	shellEscape   bool
	jobName       string
	force         bool
	quiet         bool
	jsonOutput    bool
	dryRun        bool
	watch         bool
	watchInterval time.Duration
	watchDebounce time.Duration
	insecure      bool
	entry         string
	exclude       []string
	configPath    string
}

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) == 0 {
		return 2
	}
	invokedAs := strings.ToLower(filepath.Base(args[0]))
	argv := args[1:]
	if len(argv) > 0 {
		switch argv[0] {
		case "version", "--version", "-version":
			fmt.Printf("latexmk %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
			return 0
		case "help", "--help", "-h":
			usage()
			return 0
		case "init":
			return runInit(argv[1:])
		case "meta":
			return runMeta(argv[1:], false)
		case "doctor":
			return runMeta(argv[1:], true)
		case "clean":
			return runClean(argv[1:])
		case "files":
			return runCompile(argv[1:], "", true)
		case "watch":
			argv = append([]string{"--watch"}, argv[1:]...)
		case "compile":
			argv = argv[1:]
		}
	}

	forcedEngine := ""
	switch invokedAs {
	case "xelatex", "xelatex.exe":
		forcedEngine = "xelatex"
	case "lualatex", "lualatex.exe":
		forcedEngine = "lualatex"
	case "pdflatex", "pdflatex.exe":
		forcedEngine = "pdflatex"
	}
	return runCompile(argv, forcedEngine, false)
}

func runCompile(args []string, forcedEngine string, listOnly bool) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return fail(err)
	}
	opts := compileOptions{
		server:        cfg.Server,
		token:         cfg.Token,
		projectRoot:   cfg.ProjectRoot,
		rootMode:      cfg.RootMode,
		uploadMode:    cfg.UploadMode,
		manifestFile:  cfg.ManifestFile,
		includeFiles:  append([]string(nil), cfg.IncludeFiles...),
		gitIgnore:     cfg.RespectGitIgnore,
		engine:        cfg.Engine,
		outDir:        "",
		timeout:       cfg.Timeout,
		interaction:   "nonstopmode",
		synctex:       true,
		haltOnError:   true,
		fileLineError: true,
		insecure:      cfg.InsecureSkipVerify,
		exclude:       cfg.Exclude,
		configPath:    cfg.ConfigPath,
		dryRun:        listOnly,
		watchInterval: 500 * time.Millisecond,
		watchDebounce: 500 * time.Millisecond,
	}
	if forcedEngine != "" {
		opts.engine = forcedEngine
	}
	if err := parseCompileArgs(args, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "latexmk:", err)
		fmt.Fprintln(os.Stderr, "run 'latexmk help' for usage")
		return 2
	}
	if opts.entry == "" {
		return fail(errors.New("no TeX entry file was provided"))
	}
	if err := normalizeCompilePaths(&opts, cwd); err != nil {
		return fail(err)
	}
	if opts.dryRun {
		return printManifest(opts)
	}

	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return fail(err)
	}
	c.ProjectRoot = opts.projectRoot
	c.Exclude = opts.exclude
	c.RespectGitIgnore = opts.gitIgnore
	c.UploadMode = opts.uploadMode
	c.ManifestFile = opts.manifestFile
	c.IncludeFiles = append([]string(nil), opts.includeFiles...)
	request := protocol.CompileRequest{
		ProtocolVersion: protocol.Version,
		Entry:           opts.entry,
		Engine:          opts.engine,
		Interaction:     opts.interaction,
		Synctex:         opts.synctex,
		HaltOnError:     opts.haltOnError,
		FileLineError:   opts.fileLineError,
		ShellEscape:     opts.shellEscape,
		JobName:         opts.jobName,
		Force:           opts.force,
		Quiet:           opts.quiet,
	}
	if opts.watch {
		return runWatch(c, request, opts)
	}
	out, err := compileWithTimeout(context.Background(), c, request, opts)
	return reportCompile(out, err, opts)
}

func compileWithTimeout(parent context.Context, c *client.Client, request protocol.CompileRequest, opts compileOptions) (client.CompileOutput, error) {
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()
	return c.Compile(ctx, request, opts.outDir)
}

func reportCompile(out client.CompileOutput, err error, opts compileOptions) int {
	if err != nil {
		return fail(err)
	}
	for _, warning := range out.Warnings {
		fmt.Fprintln(os.Stderr, "latexmk: warning:", warning)
	}
	if opts.jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(out.Result)
	} else {
		if !opts.quiet && len(out.Stdout) > 0 {
			_, _ = os.Stdout.Write(out.Stdout)
		}
		if len(out.Stderr) > 0 {
			_, _ = os.Stderr.Write(out.Stderr)
		}
		fmt.Fprintf(os.Stderr, "latexmk: request=%s server=%s profile=%s engine=%s duration=%dms artifacts=%d\n", out.Result.RequestID, out.Result.ServerVersion, out.Result.ImageProfile, out.Result.Engine, out.Result.DurationMS, len(out.Result.Artifacts))
		if out.Result.StdoutTruncated || out.Result.StderrTruncated {
			fmt.Fprintln(os.Stderr, "latexmk: warning: server truncated compiler output")
		}
	}
	if !out.Result.Success {
		if out.Result.Error != "" {
			fmt.Fprintln(os.Stderr, "latexmk:", out.Result.Error)
		}
		if out.Result.TimedOut {
			return 124
		}
		if out.Result.ExitCode > 0 && out.Result.ExitCode < 126 {
			return out.Result.ExitCode
		}
		return 1
	}
	return 0
}

func runWatch(c *client.Client, request protocol.CompileRequest, opts compileOptions) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	files, _, err := c.Manifest(request.Entry, request.Engine)
	if err != nil {
		return fail(fmt.Errorf("initialize watch manifest: %w", err))
	}
	fmt.Fprintf(os.Stderr, "latexmk: watching %d selected files (interval=%s debounce=%s)\n", len(files), opts.watchInterval, opts.watchDebounce)
	for {
		refreshed, _, refreshErr := c.Manifest(request.Entry, request.Engine)
		if refreshErr != nil {
			fmt.Fprintln(os.Stderr, "latexmk: warning: could not refresh manifest before watch compile:", refreshErr)
		} else {
			files = refreshed
		}
		before := files
		out, compileErr := compileWithTimeout(ctx, c, request, opts)
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "latexmk: watch stopped")
			return 0
		}
		code := reportCompile(out, compileErr, opts)
		if code != 0 {
			fmt.Fprintf(os.Stderr, "latexmk: watch compile failed with status %d; waiting for another change\n", code)
		}

		after, _, manifestErr := c.Manifest(request.Entry, request.Engine)
		if manifestErr != nil {
			fmt.Fprintln(os.Stderr, "latexmk: warning: could not refresh watch manifest:", manifestErr)
			after = before
		}
		if selectedFilesChanged(before, after) {
			files = after
			fmt.Fprintln(os.Stderr, "latexmk: selected files changed during compilation; scheduling another immutable compile")
			if !waitForContext(ctx, opts.watchDebounce) {
				fmt.Fprintln(os.Stderr, "latexmk: watch stopped")
				return 0
			}
			continue
		}
		files = after
		tracker, trackErr := projectwatch.New(watchTargets(opts, files), opts.watchInterval, opts.watchDebounce)
		if trackErr != nil {
			return fail(trackErr)
		}
		changed, waitErr := tracker.Wait(ctx)
		if waitErr != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "latexmk: watch stopped")
				return 0
			}
			return fail(waitErr)
		}
		fmt.Fprintln(os.Stderr, "latexmk: change detected:", strings.Join(changed, ", "))
	}
}

func selectedFilesChanged(before, after []projectarchive.File) bool {
	current := make(map[string]string, len(after))
	for _, file := range after {
		current[file.Path] = file.SHA256
	}
	for _, file := range before {
		if current[file.Path] != file.SHA256 {
			return true
		}
	}
	return false
}

func watchTargets(opts compileOptions, files []projectarchive.File) []projectwatch.Target {
	targets := make([]projectwatch.Target, 0, len(files)+8)
	for _, file := range files {
		targets = append(targets, projectwatch.Target{Name: file.Path, Path: file.Source})
	}
	if opts.manifestFile != "" {
		if clean, err := dependency.NormalizeExplicitManifestPath(opts.manifestFile); err == nil {
			targets = append(targets, projectwatch.Target{Name: "dependency manifest " + clean, Path: filepath.Join(opts.projectRoot, filepath.FromSlash(clean))})
		}
	}
	if !opts.gitIgnore {
		return targets
	}
	repoRoot, err := config.FindGitRoot(opts.projectRoot)
	if err != nil {
		return targets
	}
	policyPaths := make(map[string]struct{})
	for _, file := range files {
		for dir := filepath.Dir(file.Source); ; dir = filepath.Dir(dir) {
			policyPaths[filepath.Join(dir, ".gitignore")] = struct{}{}
			if dir == repoRoot || filepath.Dir(dir) == dir {
				break
			}
		}
	}
	policyPaths[filepath.Join(repoRoot, ".git", "info", "exclude")] = struct{}{}
	for policyPath := range policyPaths {
		label, relErr := filepath.Rel(opts.projectRoot, policyPath)
		if relErr != nil {
			label = policyPath
		}
		targets = append(targets, projectwatch.Target{Name: "Git policy " + filepath.ToSlash(label), Path: policyPath})
	}
	return targets
}

func waitForContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func parseCompileArgs(args []string, opts *compileOptions) error {
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(name string) (string, error) {
			if strings.Contains(a, "=") {
				return strings.SplitN(a, "=", 2)[1], nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			positionals = append(positionals, args[i+1:]...)
			i = len(args)
		case a == "--server" || strings.HasPrefix(a, "--server="):
			v, err := value("--server")
			if err != nil {
				return err
			}
			opts.server = v
		case a == "--token" || strings.HasPrefix(a, "--token="):
			v, err := value("--token")
			if err != nil {
				return err
			}
			opts.token = v
		case a == "--token-file" || strings.HasPrefix(a, "--token-file="):
			v, err := value("--token-file")
			if err != nil {
				return err
			}
			opts.token, err = config.ReadTokenFile(v)
			if err != nil {
				return err
			}
		case a == "--project-root" || strings.HasPrefix(a, "--project-root="):
			v, err := value("--project-root")
			if err != nil {
				return err
			}
			opts.projectRoot = v
		case a == "--root-mode" || strings.HasPrefix(a, "--root-mode="):
			v, err := value("--root-mode")
			if err != nil {
				return err
			}
			if v != "entry" && v != "git" {
				return fmt.Errorf("--root-mode must be entry or git, got %q", v)
			}
			opts.rootMode = v
		case a == "--upload-mode" || strings.HasPrefix(a, "--upload-mode="):
			v, err := value("--upload-mode")
			if err != nil {
				return err
			}
			if v != "auto" && v != "manifest" && v != "all" {
				return fmt.Errorf("--upload-mode must be auto, manifest, or all, got %q", v)
			}
			opts.uploadMode = v
		case a == "--manifest" || strings.HasPrefix(a, "--manifest="):
			v, err := value("--manifest")
			if err != nil {
				return err
			}
			opts.manifestFile = v
		case a == "--include-file" || strings.HasPrefix(a, "--include-file="):
			v, err := value("--include-file")
			if err != nil {
				return err
			}
			opts.includeFiles = append(opts.includeFiles, v)
		case a == "--gitignore":
			opts.gitIgnore = true
		case a == "--no-gitignore":
			opts.gitIgnore = false
		case a == "--out-dir" || strings.HasPrefix(a, "--out-dir=") || a == "-output-directory" || strings.HasPrefix(a, "-output-directory="):
			v, err := value("--out-dir")
			if err != nil {
				return err
			}
			opts.outDir = v
		case a == "--engine" || strings.HasPrefix(a, "--engine="):
			v, err := value("--engine")
			if err != nil {
				return err
			}
			opts.engine = v
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, err := value("--timeout")
			if err != nil {
				return err
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			opts.timeout = d
		case a == "--interaction" || strings.HasPrefix(a, "--interaction=") || strings.HasPrefix(a, "-interaction="):
			v, err := value("interaction")
			if err != nil {
				return err
			}
			opts.interaction = v
		case a == "--jobname" || strings.HasPrefix(a, "--jobname=") || strings.HasPrefix(a, "-jobname="):
			v, err := value("jobname")
			if err != nil {
				return err
			}
			opts.jobName = v
		case a == "--synctex" || a == "-synctex=1":
			opts.synctex = true
		case a == "--no-synctex" || a == "-synctex=0":
			opts.synctex = false
		case a == "--shell-escape" || a == "-shell-escape":
			opts.shellEscape = true
		case a == "--no-shell-escape" || a == "-no-shell-escape":
			opts.shellEscape = false
		case a == "--halt-on-error" || a == "-halt-on-error":
			opts.haltOnError = true
		case a == "--no-halt-on-error":
			opts.haltOnError = false
		case a == "--file-line-error" || a == "-file-line-error":
			opts.fileLineError = true
		case a == "--no-file-line-error":
			opts.fileLineError = false
		case a == "-xelatex" || a == "-pdfxe":
			opts.engine = "xelatex"
		case a == "-lualatex" || a == "-pdflua":
			opts.engine = "lualatex"
		case a == "-pdf" || a == "-pdflatex":
			opts.engine = "pdflatex"
		case a == "-g" || a == "-gg" || a == "--force":
			opts.force = true
		case a == "-quiet" || a == "-silent" || a == "--quiet":
			opts.quiet = true
		case a == "--json":
			opts.jsonOutput = true
		case a == "--dry-run":
			opts.dryRun = true
		case a == "--watch":
			opts.watch = true
		case a == "--watch-interval" || strings.HasPrefix(a, "--watch-interval="):
			v, err := value("--watch-interval")
			if err != nil {
				return err
			}
			opts.watchInterval, err = time.ParseDuration(v)
			if err != nil {
				return err
			}
		case a == "--watch-debounce" || strings.HasPrefix(a, "--watch-debounce="):
			v, err := value("--watch-debounce")
			if err != nil {
				return err
			}
			opts.watchDebounce, err = time.ParseDuration(v)
			if err != nil {
				return err
			}
		case a == "--insecure-skip-verify":
			opts.insecure = true
		case a == "--version":
			return errors.New("--version must be used without a compile target")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unsupported option %q; use structured options instead of arbitrary TeX flags", a)
		default:
			positionals = append(positionals, a)
		}
	}
	if len(positionals) > 1 {
		return fmt.Errorf("only one entry file is supported, got %d", len(positionals))
	}
	if len(positionals) == 1 {
		opts.entry = positionals[0]
	}
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if opts.watch && opts.watchInterval <= 0 {
		return errors.New("watch interval must be positive")
	}
	if opts.watch && opts.watchDebounce < 0 {
		return errors.New("watch debounce cannot be negative")
	}
	return nil
}

func normalizeCompilePaths(opts *compileOptions, cwd string) error {
	entryAbs := opts.entry
	if !filepath.IsAbs(entryAbs) {
		entryAbs = filepath.Join(cwd, entryAbs)
	}
	entryAbs, err := filepath.Abs(entryAbs)
	if err != nil {
		return err
	}
	entryAbs, err = filepath.EvalSymlinks(entryAbs)
	if err != nil {
		return fmt.Errorf("entry file: %w", err)
	}
	st, err := os.Stat(entryAbs)
	if err != nil {
		return fmt.Errorf("entry file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return errors.New("entry is not a regular file")
	}

	root := opts.projectRoot
	if root == "" {
		switch opts.rootMode {
		case "", "entry":
			root = filepath.Dir(entryAbs)
		case "git":
			root, err = config.FindGitRoot(filepath.Dir(entryAbs))
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid root mode %q", opts.rootMode)
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("project root: %w", err)
	}
	rel, err := filepath.Rel(root, entryAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("entry %s is outside project root %s", entryAbs, root)
	}
	opts.projectRoot = root
	opts.entry = filepath.ToSlash(rel)
	if opts.outDir == "" {
		opts.outDir = root
	}
	if !filepath.IsAbs(opts.outDir) {
		opts.outDir = filepath.Join(cwd, opts.outDir)
	}
	opts.outDir, err = filepath.Abs(opts.outDir)
	return err
}

type manifestView struct {
	ProjectRoot string                  `json:"projectRoot"`
	Entry       string                  `json:"entry"`
	UploadMode  string                  `json:"uploadMode"`
	Resolved    bool                    `json:"resolved"`
	Files       []projectarchive.File   `json:"files"`
	Stats       projectarchive.Stats    `json:"stats"`
	Diagnostics []dependency.Diagnostic `json:"diagnostics,omitempty"`
}

func printManifest(opts compileOptions) int {
	exclude := append([]string(nil), opts.exclude...)
	manifestPath := ""
	if opts.manifestFile != "" {
		var err error
		manifestPath, err = dependency.NormalizeExplicitManifestPath(opts.manifestFile)
		if err != nil {
			return fail(fmt.Errorf("manifest path: %w", err))
		}
		exclude = append(exclude, manifestPath)
	}
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{
		Root: opts.projectRoot, Exclude: exclude, RespectGitIgnore: opts.gitIgnore, MaxFiles: 20_000, MaxBytes: 2 << 30,
	})
	if err != nil {
		return fail(fmt.Errorf("build project manifest: %w", err))
	}
	var cached []string
	explicit := append([]string(nil), opts.includeFiles...)
	historyAvailable := false
	if opts.uploadMode != "all" {
		manifestFiles, manifestErr := dependency.LoadExplicitManifest(opts.projectRoot, manifestPath)
		if manifestErr != nil {
			return fail(fmt.Errorf("load explicit manifest: %w", manifestErr))
		}
		explicit = append(explicit, manifestFiles...)
	}
	if opts.uploadMode == "auto" || opts.uploadMode == "" {
		cached, historyAvailable, err = dependency.LoadCachedInputs(opts.projectRoot, opts.entry, opts.engine)
		if err != nil {
			return fail(fmt.Errorf("load dependency cache: %w", err))
		}
	}
	result, err := dependency.SelectWithOptions(opts.entry, candidates, dependency.SelectionOptions{Mode: opts.uploadMode, ExplicitFiles: explicit, CachedFiles: cached, HistoryAvailable: historyAvailable})
	if err != nil {
		return fail(fmt.Errorf("select project dependencies: %w", err))
	}
	if opts.jsonOutput {
		view := manifestView{ProjectRoot: opts.projectRoot, Entry: opts.entry, UploadMode: opts.uploadMode, Resolved: result.Resolved, Files: result.Files, Stats: result.Stats, Diagnostics: result.Diagnostics}
		if err := json.NewEncoder(os.Stdout).Encode(view); err != nil {
			return fail(err)
		}
		if !result.Resolved {
			return 1
		}
		return 0
	}
	fmt.Printf("project root: %s\nentry: %s\nupload mode: %s\nresolved: %t\nfiles: %d\nbytes: %d\n", opts.projectRoot, opts.entry, opts.uploadMode, result.Resolved, result.Stats.Files, result.Stats.Bytes)
	for _, file := range result.Files {
		fmt.Printf("%10d  %s  %s  (%s)\n", file.Size, file.SHA256, file.Path, file.Reason)
	}
	for _, diagnostic := range result.Diagnostics {
		fmt.Fprintf(os.Stderr, "latexmk: dependency: %s\n", dependency.FormatDiagnostic(diagnostic))
	}
	if !result.Resolved {
		fmt.Fprintln(os.Stderr, "latexmk: dependency discovery has unresolved references; fix them or review --upload-mode all")
		return 1
	}
	return 0
}

func runMeta(args []string, doctor bool) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return fail(err)
	}
	server, token, timeout, insecure, jsonOutput := cfg.Server, cfg.Token, cfg.Timeout, cfg.InsecureSkipVerify, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if strings.Contains(a, "=") {
				return strings.SplitN(a, "=", 2)[1], nil
			}
			if i+1 >= len(args) {
				return "", errors.New("missing option value")
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--server" || strings.HasPrefix(a, "--server="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			server = v
		case a == "--token" || strings.HasPrefix(a, "--token="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			token = v
		case a == "--token-file" || strings.HasPrefix(a, "--token-file="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			token, e = config.ReadTokenFile(v)
			if e != nil {
				return fail(e)
			}
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			timeout, e = time.ParseDuration(v)
			if e != nil {
				return fail(e)
			}
		case a == "--insecure-skip-verify":
			insecure = true
		case a == "--json":
			jsonOutput = true
		default:
			return fail(fmt.Errorf("unknown option %q", a))
		}
	}
	c, err := client.New(server, token, timeout, insecure)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if doctor {
		if err := c.Health(ctx); err != nil {
			return fail(fmt.Errorf("health check failed: %w", err))
		}
	}
	meta, err := c.Metadata(ctx)
	if err != nil {
		return fail(err)
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(meta)
		return 0
	}
	fmt.Printf("service: %s %s\nprotocol: %d\nprofile: %s\nauth: %s\ndatabase: %s\nengines: %s\n", meta.Service, meta.Version, meta.ProtocolVersion, meta.ImageProfile, meta.AuthMode, meta.Database, strings.Join(meta.Capabilities.Engines, ", "))
	for _, name := range []string{"latexmk", "xelatex", "lualatex", "pdflatex", "biber"} {
		if v := meta.Toolchain[name]; v != "" {
			fmt.Printf("%s: %s\n", name, v)
		}
	}
	if doctor {
		fmt.Println("status: healthy")
	}
	return 0
}

func runInit(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	path := filepath.Join(cwd, config.FileName)
	server := "http://127.0.0.1:8080"
	for i := 0; i < len(args); i++ {
		if args[i] == "--server" && i+1 < len(args) {
			i++
			server = args[i]
		} else if strings.HasPrefix(args[i], "--server=") {
			server = strings.SplitN(args[i], "=", 2)[1]
		} else {
			return fail(fmt.Errorf("unknown option %q", args[i]))
		}
	}
	if _, err := os.Stat(path); err == nil {
		return fail(fmt.Errorf("%s already exists", path))
	}
	if err := config.Write(path, config.FileConfig{Server: server, Engine: "xelatex", Timeout: "3m"}); err != nil {
		return fail(err)
	}
	fmt.Println(path)
	return 0
}

func runClean(args []string) int {
	entry := "main.tex"
	if len(args) > 1 {
		return fail(errors.New("clean accepts at most one entry file"))
	}
	if len(args) == 1 {
		entry = args[0]
	}
	stem := strings.TrimSuffix(filepath.Base(entry), filepath.Ext(entry))
	dir := filepath.Dir(entry)
	if dir == "." {
		dir = "."
	}
	extensions := []string{".aux", ".bbl", ".bcf", ".blg", ".fdb_latexmk", ".fls", ".log", ".out", ".run.xml", ".synctex.gz", ".toc", ".xdv"}
	removed := 0
	for _, ext := range extensions {
		p := filepath.Join(dir, stem+ext)
		if err := os.Remove(p); err == nil {
			removed++
		} else if !os.IsNotExist(err) {
			return fail(err)
		}
	}
	fmt.Printf("removed %d generated files\n", removed)
	return 0
}

func usage() {
	fmt.Print(`latexmk - remote, PaaS-hosted LaTeX compiler

Usage:
  latexmk compile [options] <main.tex>
  latexmk watch [options] <main.tex>
  latexmk [latex-compatible-options] <main.tex>
  latexmk meta [--json]
  latexmk doctor
  latexmk init [--server URL]
  latexmk clean [main.tex]
  latexmk files [options] <main.tex>
  latexmk version

Compile options:
  --server URL                 Remote server URL
  --token TOKEN                Bearer token (prefer LATEXMK_TOKEN)
  --token-file FILE            Read the bearer token from a file
  --project-root DIR           Root directory uploaded to the server
  --root-mode entry|git        Default root when --project-root is absent
  --upload-mode MODE           auto (default), manifest, or all
  --manifest FILE              Read exact project-relative files, one per line
  --include-file FILE          Add one exact project-relative file (repeatable)
  --gitignore                  Respect Git ignore rules (default)
  --no-gitignore               Include Git-ignored files unless otherwise excluded
  --out-dir DIR                Local root for returned artifacts
  --engine xelatex|lualatex|pdflatex
  --timeout 3m                 End-to-end request timeout
  --shell-escape               Request shell escape; server policy may reject it
  --jobname NAME               TeX job name
  --no-synctex                 Disable SyncTeX
  --json                       Print machine-readable result
  --dry-run                    Print the upload manifest without contacting the server
  --watch                      Recompile after selected dependency changes
  --watch-interval 500ms       Poll only selected files at this interval
  --watch-debounce 500ms       Wait for rapid edits to settle before compiling

The executable may be symlinked as xelatex, lualatex, or pdflatex.
Configuration is read from the user config, .latexmk.json, and environment variables.
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "latexmk:", err)
	return 2
}
