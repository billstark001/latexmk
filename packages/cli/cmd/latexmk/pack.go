package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	projectpack "github.com/billstark001/latexmk/packages/cli/internal/pack"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type packOptions struct {
	mode, output string
	verify       bool
}

type packView struct {
	Mode          string                `json:"mode"`
	Output        string                `json:"output"`
	Entry         string                `json:"entry"`
	Engine        string                `json:"engine"`
	Files         []projectarchive.File `json:"files"`
	RequiresBuild bool                  `json:"requiresBuild"`
	Verified      bool                  `json:"verified"`
	BuildJobID    string                `json:"buildJobId,omitempty"`
	Warnings      []string              `json:"warnings,omitempty"`
}

func parsePackArgs(args []string) (packOptions, []string, error) {
	options := packOptions{mode: "default"}
	var common []string
	for i := 0; i < len(args); i++ {
		name, value, supplied := strings.Cut(args[i], "=")
		switch name {
		case "--":
			common = append(common, args[i:]...)
			i = len(args)
		case "--mode", "--output":
			if !supplied {
				i++
				if i >= len(args) {
					return options, nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if value == "" {
				return options, nil, fmt.Errorf("%s requires a nonempty value", name)
			}
			if name == "--mode" {
				options.mode = value
			} else {
				options.output = value
			}
		case "--verify":
			if supplied {
				return options, nil, errors.New("--verify does not accept a value")
			}
			options.verify = true
		case "--local-cache",
			"--server-cache",
			"--server-cache-ttl",
			"--force",
			"-g",
			"-gg",
			"--synctex",
			"-synctex",
			"--no-synctex":
			return options, nil, fmt.Errorf("%s is not a pack option; pack verification uses a cold build", name)
		default:
			common = append(common, args[i])
		}
	}
	return options, common, projectpack.ValidateMode(options.mode)
}

func runPack(args []string) int {
	jsonOutput := hasJSONFlag(args)
	fail := func(err error) int { return failAgent("pack", jsonOutput, err) }
	failArgs := func(err error) int { return failAgentArguments("pack", jsonOutput, err) }
	packing, common, err := parsePackArgs(args)
	if err != nil {
		return failArgs(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, common, err := config.LoadArgs(cwd, common)
	if err != nil {
		return failArgs(err)
	}
	opts := optionsFromConfig(cfg, false)
	if err := parseCompileArgs(common, &opts); err != nil {
		return failArgs(err)
	}
	if opts.watch || opts.detach || opts.target == "all" || opts.explain != "" {
		return failArgs(errors.New("pack supports one entry/target and cannot watch, detach, or explain"))
	}
	if opts.target == "" && opts.entry == "" {
		opts.target, err = implicitTarget(cfg)
		if err != nil {
			return failArgs(err)
		}
	}
	if opts.target != "" {
		if err := applyBuildTarget(&opts, cfg, common); err != nil {
			return failArgs(err)
		}
	}
	if opts.entry == "" {
		return failArgs(errors.New("no TeX entry file was provided"))
	}
	if err := normalizeCompilePaths(&opts, cwd); err != nil {
		return fail(err)
	}
	stem := strings.TrimSuffix(filepath.Base(opts.entry), filepath.Ext(opts.entry))
	if packing.mode == "arxiv" && (opts.shellEscape || (opts.jobName != "" && opts.jobName != stem)) {
		return failArgs(errors.New("arxiv mode requires shell escape disabled and the entry's default job name"))
	}
	if packing.output == "" {
		packing.output = filepath.Join(opts.outDir, stem+"-"+packing.mode+".zip")
	}
	packing.output, err = filepath.Abs(packing.output)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(filepath.Ext(packing.output), ".zip") {
		return failArgs(errors.New("pack output must have a .zip extension"))
	}
	// An existing output can never become its own input in upload mode all.
	opts.denyFiles = append(opts.denyFiles, packing.output)
	c := selectionClient(opts)
	request := requestFromOptions(opts)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	captured, err := c.FreezeSnapshot(ctx, request, nil, protocol.Metadata{}, nil)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = captured.Close() }()
	sources, err := projectpack.Sources(captured.Files, opts.entry, packing.mode)
	if err != nil {
		return fail(err)
	}
	view := packView{Mode: packing.mode, Output: packing.output, Entry: opts.entry, Engine: opts.engine,
		Files: sources, RequiresBuild: packing.verify || packing.mode == "arxiv", Warnings: captured.Warnings}
	if opts.dryRun {
		return reportPack(view, jsonOutput, true)
	}
	files := sources
	if view.RequiresBuild {
		if err := cfg.Authenticate(opts.projectRoot); err != nil {
			return fail(err)
		}
		connected, err := client.New(opts.server, cfg.Token, opts.timeout, opts.insecure)
		if err != nil {
			return fail(err)
		}
		connected.ProjectRoot, connected.ProjectID = opts.projectRoot, opts.projectID
		meta, err := connected.Metadata(ctx)
		if err != nil {
			return fail(err)
		}
		output, err := os.MkdirTemp("", "latexmk-pack-build-")
		if err != nil {
			return fail(err)
		}
		defer func() { _ = os.RemoveAll(output) }()
		// Pack builds are cold and retain auxiliaries only for immediate download.
		request.Force, request.Synctex = true, false
		request.Auxiliary = protocol.AuxiliaryOptions{Local: "output", Server: "none"}
		build, err := connected.CompileCaptured(ctx, request, captured.Frozen, output, meta)
		if err != nil {
			return fail(err)
		}
		if !build.Result.Success {
			return fail(packBuildError("pack build", build))
		}
		view.BuildJobID = build.Result.RequestID
		if packing.mode == "arxiv" {
			generated, err := submissionFiles(output, opts, build.Result.Artifacts)
			if err != nil {
				return fail(err)
			}
			files, err = projectpack.Merge(sources, generated)
			if err != nil {
				return fail(err)
			}
		}
		// Validate the exact final package, including the removal of arXiv
		// intermediates. No local edit or previous live result enters this build.
		bundle, err := projectarchive.Freeze(ctx, files, projectpack.MaxFiles, projectpack.MaxBytes, nil)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = bundle.Close() }()
		files = bundle.Files
		if packing.mode == "arxiv" {
			verification, err := connected.CompileCaptured(
				ctx,
				request,
				bundle,
				filepath.Join(output, "verified"),
				meta,
			)
			if err != nil {
				return fail(err)
			}
			if !verification.Result.Success {
				return fail(packBuildError("arxiv package verification", verification))
			}
		}
		view.Verified = true
	}
	if err := projectpack.Write(ctx, packing.output, files); err != nil {
		return fail(err)
	}
	view.Files, view.RequiresBuild = files, false
	return reportPack(view, jsonOutput, false)
}

func packBuildError(stage string, build client.CompileOutput) error {
	for _, log := range [][]byte{build.Stdout, build.Stderr} {
		if len(log) > 0 {
			fmt.Fprint(os.Stderr, terminalText(string(log)))
		}
	}
	if build.Result.TimedOut {
		return fmt.Errorf("%s: %w", stage, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s failed (job %s): %s", stage, build.Result.RequestID, build.Result.Error)
}

func submissionFiles(output string, opts compileOptions, artifacts []protocol.Artifact) ([]projectarchive.File, error) {
	// Apply the same sensitive-path deny policy to generated exports. Only
	// allowlisted, hash-verified members of this job's result are eligible.
	allowed, _, err := projectarchive.Manifest(projectarchive.Options{Root: output,
		Exclude: config.DefaultDeny(), MaxFiles: projectpack.MaxFiles, MaxBytes: projectpack.MaxBytes})
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]projectarchive.File)
	for _, file := range allowed {
		byPath[file.Path] = file
	}
	var files []projectarchive.File
	for _, artifact := range artifacts {
		if !projectpack.SubmissionArtifact(artifact.Path) {
			continue
		}
		file, ok := byPath[artifact.Path]
		if !ok || file.Size != artifact.Size || file.SHA256 != artifact.SHA256 {
			return nil, fmt.Errorf("submission artifact %q is missing, denied, or changed", artifact.Path)
		}
		file.Reason = "generated by captured pack build"
		files = append(files, file)
	}
	return projectpack.Sources(files, opts.entry, "arxiv")
}

func reportPack(view packView, jsonOutput, dryRun bool) int {
	if jsonOutput {
		if err := writeAgentJSON("pack", view); err != nil {
			return fail(err)
		}
		return 0
	}
	if dryRun {
		fmt.Printf("pack preview (%s): %s\n", view.Mode, view.Output)
	} else {
		fmt.Printf("packed %d files (%s): %s\n", len(view.Files), view.Mode, view.Output)
	}
	for _, file := range view.Files {
		fmt.Printf("%10d  %s  %s\n", file.Size, file.SHA256, file.Path)
	}
	for _, warning := range view.Warnings {
		fmt.Fprintln(os.Stderr, "latexmk: warning:", terminalText(warning))
	}
	if dryRun && view.RequiresBuild {
		fmt.Println("source preview only; remote build and generated submission files are pending")
	}
	return 0
}
