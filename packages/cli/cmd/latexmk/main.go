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
	version   = "0.3.1"
	commit    = "unknown"
	buildDate = "unknown"
)

type compileOptions struct {
	explain       string
	ignoreFiles   []string
	denyFiles     []string
	unmatchedGlob string
	target        string
	pdfExport     string
	auxiliary     protocol.AuxiliaryOptions
	server        string
	token         string
	projectRoot   string
	projectID     string
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
	detach        bool
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
		case "cache":
			return runCache(argv[1:])
		case "remote-clean":
			return runRemoteClean(argv[1:])
		case "remote":
			if len(argv) > 1 && argv[1] == "clean" {
				return runRemoteClean(argv[2:])
			}
			return fail(errors.New("remote currently supports only 'clean'"))
		case "jobs":
			return runJobs(argv[1:])
		case "logs":
			return runLogs(argv[1:])
		case "diagnostics":
			return runDiagnostics(argv[1:])
		case "artifacts":
			return runArtifacts(argv[1:])
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
	originalArgs := append([]string{}, args...)
	if listOnly {
		originalArgs = append(originalArgs, "--dry-run")
	}
	detachedJSON := hasJSONFlag(args) && hasDetachFlag(args)
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent("compile.start", detachedJSON, err)
	}
	cfg, args, err := config.LoadArgs(cwd, args)
	if err != nil {
		return failAgentArguments("compile.start", detachedJSON, err)
	}
	opts := compileOptions{
		ignoreFiles: cfg.IgnoreFiles, denyFiles: cfg.DenyFiles, unmatchedGlob: cfg.UnmatchedGlob,
		auxiliary:     cfg.Auxiliary,
		server:        cfg.Server,
		token:         cfg.Token,
		projectRoot:   cfg.ProjectRoot,
		projectID:     cfg.ProjectID,
		rootMode:      cfg.RootMode,
		uploadMode:    cfg.UploadMode,
		manifestFile:  cfg.ManifestFile,
		includeFiles:  append([]string(nil), cfg.IncludeFiles...),
		gitIgnore:     cfg.RespectGitIgnore,
		engine:        cfg.Engine,
		outDir:        cfg.OutDir,
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
		if detachedJSON {
			return failAgentArguments("compile.start", true, err)
		}
		fmt.Fprintln(os.Stderr, "latexmk:", err)
		fmt.Fprintln(os.Stderr, "run 'latexmk help' for usage")
		return 2
	}
	if opts.target != "" {
		if opts.target == "all" {
			return runAllTargets(originalArgs, cfg)
		}
		target, ok := cfg.Targets[opts.target]
		if !ok {
			return fail(fmt.Errorf("unknown target %q", opts.target))
		}
		if opts.entry != "" {
			return fail(errors.New("--target cannot be combined with an entry"))
		}
		opts.entry, opts.pdfExport = target.Entry, target.PDF
		if cfg.ConfigPath != "" && !filepath.IsAbs(opts.entry) {
			opts.entry = filepath.Join(filepath.Dir(cfg.ConfigPath), opts.entry)
		}
		if target.Engine != "" && !hasOption(args, "--engine") {
			opts.engine = target.Engine
		}
		if target.OutDir != "" && !hasOption(args, "--out-dir") && !hasOption(args, "-output-directory") {
			opts.outDir = target.OutDir
		}
		opts.includeFiles = append(opts.includeFiles, target.IncludeFiles...)
	}
	if opts.entry == "" {
		err := errors.New("no TeX entry file was provided")
		if opts.detach {
			return failAgentArguments("compile.start", opts.jsonOutput, err)
		}
		return fail(err)
	}
	if opts.detach && (opts.watch || opts.dryRun || listOnly) {
		return failAgentArguments(
			"compile.start",
			opts.jsonOutput,
			errors.New("--detach cannot be combined with --watch, --dry-run, or files"),
		)
	}
	if err := normalizeCompilePaths(&opts, cwd); err != nil {
		if opts.detach {
			return failAgentArguments("compile.start", opts.jsonOutput, err)
		}
		return fail(err)
	}
	if opts.dryRun {
		return printManifest(opts)
	}

	if err := cfg.Authenticate(opts.projectRoot); err != nil {
		return fail(err)
	}
	opts.token = cfg.Token
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		if opts.detach {
			return failAgentArguments("compile.start", opts.jsonOutput, err)
		}
		return fail(err)
	}
	c.ProjectRoot = opts.projectRoot
	c.ProjectID = opts.projectID
	if c.ProjectID == "" {
		resolution, resolveErr := client.ResolveProjectIDWithStatus(opts.projectRoot, true)
		if resolveErr != nil {
			return fail(resolveErr)
		}
		c.ProjectID = resolution.ID
		if resolution.Created {
			fmt.Fprintln(
				os.Stderr,
				"latexmk: created a local project ID in .latexmk-cache; run 'latexmk cache ignore' in Git projects",
			)
		}
	}
	c.IgnoreFiles, c.DenyFiles, c.UnmatchedGlob = opts.ignoreFiles, append(
		opts.denyFiles,
		cfg.DenyFiles...), opts.unmatchedGlob
	c.Exclude = opts.exclude
	c.RespectGitIgnore = opts.gitIgnore
	c.UploadMode = opts.uploadMode
	c.ManifestFile = opts.manifestFile
	c.IncludeFiles = append([]string(nil), opts.includeFiles...)
	request := protocol.CompileRequest{
		Auxiliary:       opts.auxiliary,
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
	if opts.detach {
		return runDetachedCompile(c, request, opts)
	}
	out, err := compileWithTimeout(context.Background(), c, request, opts)
	return reportCompile(out, err, opts)
}

type compileStartData struct {
	Job      protocol.Job `json:"job"`
	Warnings []string     `json:"warnings,omitempty"`
}

func runDetachedCompile(c *client.Client, request protocol.CompileRequest, opts compileOptions) int {
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	out, err := c.StartCompile(ctx, request)
	if err != nil {
		return failAgent("compile.start", opts.jsonOutput, err)
	}
	if opts.jsonOutput {
		if err := writeAgentJSON("compile.start", compileStartData{Job: out.Job, Warnings: out.Warnings}); err != nil {
			return fail(err)
		}
		return 0
	}
	for _, warning := range out.Warnings {
		fmt.Fprintln(os.Stderr, "latexmk: warning:", warning)
	}
	fmt.Printf(
		"job ID: %s\nproject ID: %s\nsnapshot ID: %s\nstatus: %s\n",
		out.Job.ID,
		out.Job.ProjectID,
		out.Job.SnapshotID,
		out.Job.Status,
	)
	return 0
}

func compileWithTimeout(
	parent context.Context,
	c *client.Client,
	request protocol.CompileRequest,
	opts compileOptions,
) (client.CompileOutput, error) {
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()
	out, err := c.Compile(ctx, request, opts.outDir)
	if err == nil {
		err = exportPDF(opts, out)
	}
	return out, err
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
		if cache := out.Result.CompileCache; cache != nil {
			fmt.Fprintf(
				os.Stderr,
				"latexmk: compile cache=%s restored=%d stored=%d reason=%s\n",
				cache.Status,
				cache.RestoredFiles,
				cache.StoredFiles,
				cache.Reason,
			)
			if cache.Warning != "" {
				fmt.Fprintln(os.Stderr, "latexmk: compile cache:", cache.Warning)
			}
		}
		fmt.Fprintf(
			os.Stderr,
			"latexmk: request=%s server=%s profile=%s engine=%s duration=%dms artifacts=%d\n",
			out.Result.RequestID,
			out.Result.ServerVersion,
			out.Result.ImageProfile,
			out.Result.Engine,
			out.Result.DurationMS,
			len(out.Result.Artifacts),
		)
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
	fmt.Fprintf(
		os.Stderr,
		"latexmk: watching %d selected files (interval=%s debounce=%s)\n",
		len(files),
		opts.watchInterval,
		opts.watchDebounce,
	)
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
			fmt.Fprintln(
				os.Stderr,
				"latexmk: selected files changed during compilation; scheduling another immutable compile",
			)
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
		tracker.Refresh = func() ([]projectwatch.Target, error) {
			selection, err := c.SelectionPaths(request.Entry, request.Engine)
			if err != nil {
				return nil, err
			}
			return watchTargets(opts, selection.Files), nil
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
	if len(before) != len(after) {
		return true
	}
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
			targets = append(
				targets,
				projectwatch.Target{
					Name: "dependency manifest " + clean,
					Path: filepath.Join(opts.projectRoot, filepath.FromSlash(clean)),
				},
			)
		}
	}
	if opts.manifestFile == "" && opts.uploadMode == "manifest" && len(opts.includeFiles) == 0 {
		for _, name := range []string{".latexmk-manifest", ".latexmk-files"} {
			targets = append(
				targets,
				projectwatch.Target{Name: "dependency manifest " + name, Path: filepath.Join(opts.projectRoot, name)},
			)
		}
	}
	names := opts.ignoreFiles
	if names == nil {
		names = []string{".latexmkignore"}
	}
	for _, name := range names {
		targets = append(
			targets,
			projectwatch.Target{Name: "ignore policy " + name, Path: filepath.Join(opts.projectRoot, name)},
		)
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
	if globalExcludes, ok := effectiveGitExcludesFile(repoRoot); ok {
		policyPaths[globalExcludes] = struct{}{}
	}
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
		case a == "--ignore-file" || strings.HasPrefix(a, "--ignore-file="):
			v, err := value("--ignore-file")
			if err != nil {
				return err
			}
			if opts.ignoreFiles == nil {
				opts.ignoreFiles = []string{}
			}
			opts.ignoreFiles = append(opts.ignoreFiles, v)
		case a == "--no-ignore-files":
			opts.ignoreFiles = []string{}
		case a == "--explain" || strings.HasPrefix(a, "--explain="):
			v, err := value("--explain")
			if err != nil {
				return err
			}
			opts.explain = v
			opts.dryRun = true
		case a == "--unmatched-glob" || strings.HasPrefix(a, "--unmatched-glob="):
			v, err := value("--unmatched-glob")
			if err != nil {
				return err
			}
			if v != "error" && v != "warn" && v != "ignore" {
				return errors.New("--unmatched-glob must be error, warn, or ignore")
			}
			opts.unmatchedGlob = v
		case a == "--local-cache" || strings.HasPrefix(a, "--local-cache="):
			v, err := value("--local-cache")
			if err != nil {
				return err
			}
			if v != "none" && v != "cache" && v != "output" {
				return errors.New("--local-cache must be none, cache, or output")
			}
			opts.auxiliary.Local = v
		case a == "--server-cache-ttl" || strings.HasPrefix(a, "--server-cache-ttl="):
			v, err := value("--server-cache-ttl")
			if err != nil {
				return err
			}
			ttl, err := time.ParseDuration(v)
			if err != nil || ttl <= 0 {
				return errors.New("--server-cache-ttl must be a positive duration")
			}
			opts.auxiliary.ServerTTL = v
		case a == "--target" || strings.HasPrefix(a, "--target="):
			v, err := value("--target")
			if err != nil {
				return err
			}
			opts.target = v
		case a == "--project-root" || strings.HasPrefix(a, "--project-root="):
			v, err := value("--project-root")
			if err != nil {
				return err
			}
			opts.projectRoot = v
		case a == "--project-id" || strings.HasPrefix(a, "--project-id="):
			v, err := value("--project-id")
			if err != nil {
				return err
			}
			opts.projectID = v
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
			if v == "ignore" {
				v = "all"
			}
			if v != "auto" && v != "manifest" && v != "all" {
				return fmt.Errorf("--upload-mode must be auto, manifest, or all, got %q", v)
			}
			opts.uploadMode = v
		case a == "--server-cache" || strings.HasPrefix(a, "--server-cache="):
			v, err := value("--server-cache")
			if err != nil {
				return err
			}
			if v != "none" && v != "reuse" && v != "retain" {
				return errors.New("--server-cache must be none, retain, or reuse")
			}
			opts.auxiliary.Server = v
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
		case a == "--detach":
			opts.detach = true
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

func hasDetachFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--detach" {
			return true
		}
	}
	return false
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
	if opts.explain != "" {
		deny := append([]string{}, opts.denyFiles...)
		if opts.manifestFile != "" {
			deny = append(deny, filepath.Join(opts.projectRoot, opts.manifestFile))
		}
		explanation, err := projectarchive.Explain(
			projectarchive.Options{
				Root:             opts.projectRoot,
				Exclude:          opts.exclude,
				IgnoreFiles:      opts.ignoreFiles,
				DenyFiles:        deny,
				RespectGitIgnore: opts.gitIgnore,
			},
			opts.explain,
		)
		if err != nil {
			return fail(err)
		}
		if opts.jsonOutput {
			if err := json.NewEncoder(os.Stdout).Encode(explanation); err != nil {
				return fail(err)
			}
		} else {
			fmt.Printf("%s: %s\n", explanation.Path, explanation.Reason)
		}
		return 0
	}
	result, err := selectionClient(opts).Selection(opts.entry, opts.engine, nil)
	if err != nil {
		return fail(err)
	}
	if opts.jsonOutput {
		view := manifestView{
			ProjectRoot: opts.projectRoot,
			Entry:       opts.entry,
			UploadMode:  opts.uploadMode,
			Resolved:    result.Resolved,
			Files:       result.Files,
			Stats:       result.Stats,
			Diagnostics: result.Diagnostics,
		}
		if err := json.NewEncoder(os.Stdout).Encode(view); err != nil {
			return fail(err)
		}
		if !result.Resolved {
			return 1
		}
		return 0
	}
	fmt.Printf(
		"project root: %s\nentry: %s\nupload mode: %s\nresolved: %t\nfiles: %d\nbytes: %d\n",
		opts.projectRoot,
		opts.entry,
		opts.uploadMode,
		result.Resolved,
		result.Stats.Files,
		result.Stats.Bytes,
	)
	for _, file := range result.Files {
		fmt.Printf("%10d  %s  %s  (%s)\n", file.Size, file.SHA256, file.Path, file.Reason)
	}
	for _, diagnostic := range result.Diagnostics {
		fmt.Fprintf(os.Stderr, "latexmk: dependency: %s\n", dependency.FormatDiagnostic(diagnostic))
	}
	if !result.Resolved {
		fmt.Fprintln(
			os.Stderr,
			"latexmk: dependency discovery has unresolved references; fix them or review --upload-mode all",
		)
		return 1
	}
	return 0
}

func runMeta(args []string, doctor bool) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, args, err := config.LoadArgs(cwd, args)
	if err != nil {
		return fail(err)
	}
	if doctor {
		reportDoctorProjectCache(cwd, cfg.ProjectRoot, hasJSONFlag(args))
	}
	server, timeout, insecure, jsonOutput := cfg.Server, cfg.Timeout, cfg.InsecureSkipVerify, false
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
	if err := cfg.Authenticate(cfg.ProjectRoot); err != nil {
		return fail(err)
	}
	if doctor && !jsonOutput {
		fmt.Fprintln(os.Stderr, "authentication:", cfg.TokenSource)
	}
	c, err := client.New(server, cfg.Token, timeout, insecure)
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
	fmt.Printf(
		"service: %s %s\nprotocol: %d\nprofile: %s\nauth: %s\ndatabase: %s\nengines: %s\n",
		meta.Service,
		meta.Version,
		meta.ProtocolVersion,
		meta.ImageProfile,
		meta.AuthMode,
		meta.Database,
		strings.Join(meta.Capabilities.Engines, ", "),
	)
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
	if err := config.Write(
		path,
		config.FileConfig{
			Server:    config.ServerSources{config.LiteralSource(server)},
			Engine:    "xelatex",
			Timeout:   "3m",
			Auxiliary: protocol.AuxiliaryOptions{Local: "none", Server: "none"},
		},
	); err != nil {
		return fail(err)
	}
	fmt.Println(path)
	fmt.Fprintln(os.Stderr, "latexmk: recommended: run 'latexmk cache ignore' to protect the local project identity")
	fmt.Fprintln(
		os.Stderr,
		"latexmk: warning: 'git clean -fdX' deletes ignored cache files; the next compile creates a new project ID",
	)
	return 0
}

func reportDoctorProjectCache(cwd, configuredRoot string, jsonOutput bool) {
	root := configuredRoot
	if root == "" {
		root = cwd
	}
	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "latexmk: doctor: could not inspect project cache Git policy: %v\n", err)
		return
	}
	status, err := client.InspectProjectCacheGitIgnore(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "latexmk: doctor: could not inspect project cache Git policy: %v\n", err)
		return
	}
	if !status.InWorkTree {
		if !jsonOutput {
			fmt.Println("project cache Git ignore: not applicable (not a Git work tree)")
		}
		return
	}
	if status.Ignored {
		if !jsonOutput {
			fmt.Println("project cache Git ignore: configured")
		}
		return
	}
	fmt.Fprintln(os.Stderr, "latexmk: doctor: "+client.ProjectCacheGitAdvice)
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
	extensions := []string{
		".aux",
		".bbl",
		".bcf",
		".blg",
		".fdb_latexmk",
		".fls",
		".log",
		".out",
		".run.xml",
		".synctex.gz",
		".toc",
		".xdv",
	}
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

type remoteCleanOptions struct {
	server          string
	token           string
	timeout         time.Duration
	insecure        bool
	projectRoot     string
	projectID       string
	scope           string
	planID          string
	yes             bool
	dryRun          bool
	jsonOutput      bool
	legacyProjectID bool
}

func parseRemoteCleanArgs(args []string, opts *remoteCleanOptions) error {
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
		var err error
		switch {
		case a == "--server" || strings.HasPrefix(a, "--server="):
			opts.server, err = value("--server")
		case a == "--insecure-skip-verify":
			opts.insecure = true
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			var raw string
			raw, err = value("--timeout")
			if err == nil {
				opts.timeout, err = time.ParseDuration(raw)
			}
		case a == "--project-root" || strings.HasPrefix(a, "--project-root="):
			opts.projectRoot, err = value("--project-root")
		case a == "--project-id" || strings.HasPrefix(a, "--project-id="):
			opts.projectID, err = value("--project-id")
		case a == "--legacy-project-id":
			opts.legacyProjectID = true
		case a == "--scope" || strings.HasPrefix(a, "--scope="):
			opts.scope, err = value("--scope")
		case a == "--plan-id" || strings.HasPrefix(a, "--plan-id="):
			opts.planID, err = value("--plan-id")
		case a == "--yes":
			opts.yes = true
		case a == "--dry-run":
			opts.dryRun = true
		case a == "--json":
			opts.jsonOutput = true
		default:
			return fmt.Errorf("unknown option %q", a)
		}
		if err != nil {
			return err
		}
	}
	if opts.yes {
		if opts.dryRun {
			return errors.New("--yes and --dry-run cannot be used together")
		}
		if !cleanupPlanIDPattern.MatchString(opts.planID) {
			return errors.New("remote clean --yes requires a valid --plan-id from a preview")
		}
		if opts.scope != "" {
			return errors.New("do not pass --scope when applying a remote cleanup plan")
		}
		return nil
	}
	if opts.planID != "" {
		return errors.New("--plan-id requires --yes")
	}
	if opts.scope != "results" && opts.scope != "snapshot" && opts.scope != "project" && opts.scope != "cache" {
		return errors.New("--scope must be results, snapshot, cache, or project")
	}
	return nil
}

func runRemoteClean(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, args, err := config.LoadArgs(cwd, args)
	if err != nil {
		return fail(err)
	}
	opts := remoteCleanOptions{
		server:      cfg.Server,
		token:       cfg.Token,
		timeout:     cfg.Timeout,
		insecure:    cfg.InsecureSkipVerify,
		projectRoot: cfg.ProjectRoot,
		projectID:   cfg.ProjectID,
	}
	if err := parseRemoteCleanArgs(args, &opts); err != nil {
		return fail(err)
	}
	if opts.projectRoot == "" {
		opts.projectRoot = cwd
	}
	opts.projectRoot, err = filepath.Abs(opts.projectRoot)
	if err == nil {
		opts.projectRoot, err = filepath.EvalSymlinks(opts.projectRoot)
	}
	if err != nil {
		return fail(fmt.Errorf("project root: %w", err))
	}
	if opts.legacyProjectID {
		if opts.projectID != "" {
			return fail(errors.New("--legacy-project-id cannot be combined with a configured or explicit project ID"))
		}
		opts.projectID, err = client.LegacyProjectID(opts.projectRoot)
	} else if opts.projectID == "" {
		opts.projectID, err = client.ResolveProjectID(opts.projectRoot, false)
		if errors.Is(err, client.ErrProjectIDNotFound) {
			return fail(
				errors.New(
					"this project has no local project ID; compile it once, or use --project-id/--legacy-project-id to clean older data",
				),
			)
		}
	}
	if err != nil {
		return fail(err)
	}
	if err := cfg.Authenticate(opts.projectRoot); err != nil {
		return fail(err)
	}
	opts.token = cfg.Token
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return fail(err)
	}
	var plan remoteCleanupPlan
	var planPath string
	if opts.yes {
		plan, planPath, err = loadRemoteCleanupPlan(opts.planID)
		if err != nil {
			return fail(err)
		}
		if !time.Now().Before(plan.ExpiresAt) {
			return fail(errors.New("remote cleanup plan has expired; create a new preview"))
		}
		if plan.Server != c.BaseURL || plan.ProjectID != opts.projectID {
			return fail(errors.New("remote cleanup plan belongs to a different server or project"))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	meta, err := c.Metadata(ctx)
	if err != nil {
		return fail(err)
	}
	if !meta.Capabilities.RemoteCleanup {
		return fail(errors.New("server does not advertise remote cleanup support"))
	}
	if !opts.yes {
		report, err := c.CleanupProject(ctx, opts.projectID, opts.scope)
		if err != nil {
			return fail(err)
		}
		plan, err := createRemoteCleanupPlan(c.BaseURL, opts.projectID, opts.scope, report)
		if err != nil {
			return fail(err)
		}
		if opts.jsonOutput {
			_ = json.NewEncoder(
				os.Stdout,
			).Encode(
				remoteCleanupOutput{PlanID: plan.ID, ExpiresAt: &plan.ExpiresAt, Report: report},
			)
			return 0
		}
		writeRemoteCleanupReport(report)
		fmt.Printf(
			"plan ID: %s\nexpires: %s\npreview only; apply with --plan-id %s --yes\n",
			plan.ID,
			plan.ExpiresAt.Format(time.RFC3339),
			plan.ID,
		)
		return 0
	}
	report, err := c.CleanupProjectWithPlan(ctx, plan.ProjectID, plan.Scope, plan.PlanDigest)
	if err != nil {
		return fail(err)
	}
	if report.ProjectID != plan.ProjectID || report.Scope != plan.Scope || report.DryRun ||
		report.PlanDigest != plan.PlanDigest {
		return fail(errors.New("server returned an inconsistent cleanup result"))
	}
	if err := consumeRemoteCleanupPlan(planPath); err != nil {
		return fail(fmt.Errorf("remote cleanup succeeded but the local plan could not be consumed: %w", err))
	}
	if opts.jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(remoteCleanupOutput{PlanID: plan.ID, Report: report})
		return 0
	}
	fmt.Printf("plan ID: %s\n", plan.ID)
	writeRemoteCleanupReport(report)
	fmt.Printf("reclaimed bytes: %d\n", report.ReclaimedBytes)
	return 0
}

func writeRemoteCleanupReport(report protocol.CleanupReport) {
	if report.Scope == "cache" || report.Scope == "project" {
		fmt.Printf("compile caches: %d (%d bytes)\n", report.CompileCaches, report.CompileCacheBytes)
	}
	fmt.Printf("project ID: %s\nscope: %s\ndry run: %t\n", report.ProjectID, report.Scope, report.DryRun)
	if report.Scope == "snapshot" || report.Scope == "project" {
		fmt.Printf(
			"snapshot: %t (%d files, %d bytes)\n",
			report.SnapshotPresent,
			report.SnapshotFiles,
			report.SnapshotBytes,
		)
	}
	if report.Scope == "results" || report.Scope == "project" {
		fmt.Printf("results: %d (%d bytes)\n", report.Results, report.ResultBytes)
	}
	if report.Scope == "project" {
		fmt.Printf("terminal jobs: %d\n", report.Jobs)
	}
	if len(report.ActiveJobs) > 0 {
		fmt.Printf("active jobs (not deleted): %s\n", strings.Join(report.ActiveJobs, ", "))
	}
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
  latexmk cache ignore [--project-root DIR] [--json]
  latexmk cache clean [--project-root DIR] [--json]
  latexmk --target NAME|all [options]
  latexmk remote clean --scope results|snapshot|cache|project [--dry-run] [--json]
  latexmk remote clean --plan-id PLAN_ID --yes [--json]
  latexmk jobs list [--limit 50] [--json]
  latexmk jobs show JOB_ID [--json]
  latexmk jobs cancel JOB_ID [--json]
  latexmk logs JOB_ID [--source all|stdout|stderr|compiler] [--tail 200] [--max-bytes 65536] [--json]
  latexmk diagnostics JOB_ID [--json]
  latexmk artifacts list JOB_ID [--json]
  latexmk artifacts get JOB_ID ARTIFACT_ID [--out-dir DIR] [--json]
  latexmk files [options] <main.tex>
  latexmk version

Compile options:
  --server URL                 Remote server URL
  --token TOKEN                Bearer token (prefer LATEXMK_TOKEN)
  --token-file FILE            Read the bearer token from a file
  --token-mode MODE            auto (default), env, file, or none
  --env-file FILE              Read dotenv settings (default .env.latexmk)
  --no-env-file                Disable dotenv loading
  --project-root DIR           Root directory uploaded to the server
  --project-id ID              Override the persisted local project identity
  --root-mode entry|git        Default root when --project-root is absent
  --upload-mode MODE           auto (default), manifest, all (alias: ignore)
  --server-cache MODE          none, retain, or reuse; --force starts clean
  --server-cache-ttl DURATION   Requested auxiliary retention, capped by server limits
  --local-cache MODE           none, cache, or output (legacy JSON preserves output)
  --manifest FILE              Read project-relative paths/globs, one per line
  --include-file PATTERN       Add project-relative paths/globs (repeatable)
  --unmatched-glob MODE        error (default), warn, or ignore
  --ignore-file FILE           Additional Git-style ignore file (repeatable)
  --no-ignore-files            Disable custom ignore files
  --explain PATH               Explain upload policy without reading excluded contents
  --target NAME|all            Compile configured build targets and export PDFs
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
  --detach                     Return after creating an immutable queued job
  --watch                      Recompile after selected dependency changes
  --watch-interval 500ms       Refresh selection and poll selected files
  --watch-debounce 500ms       Wait for rapid edits to settle before compiling

The executable may be symlinked as xelatex, lualatex, or pdflatex.
Configuration is read from user/project JSON, .env.latexmk, environment and CLI flags.

Remote cleanup previews create a ten-minute local plan. Apply that exact plan
with --plan-id PLAN_ID --yes. Use --legacy-project-id only for data created by
older path-derived project IDs.
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "latexmk:", err)
	return 2
}
