package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/argv"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	sharedresources "github.com/billstark001/latexmk/packages/shared/resources"
)

var (
	version   = "0.4.3"
	commit    = "unknown"
	buildDate = "unknown"
)

type compileOptions struct {
	resourceSpecs       map[string]sharedresources.Spec
	resourceOptions     sharedresources.Options
	resourceIDs         []string
	resourceAddIDs      []string
	resourceRemoveIDs   []string
	resourceExplicit    bool
	resourceCapture     *cliresources.Captured
	requires            sharedresources.Requirements
	environment         string
	environmentExplicit bool
	publications        map[string]sharedresources.Publication
	explain             string
	ignoreFiles         []string
	denyFiles           []string
	unmatchedGlob       string
	target              string
	pdfExport           string
	pdfTemplate         string
	pdfKeepLast         *int
	nameScope           string
	nameTimeZone        string
	naming              *pdfNamingPlan
	invocation          *nameInvocation
	generated           *generatedPaths
	exportLease         *nameReservation
	auxiliary           protocol.AuxiliaryOptions
	server              string
	token               string
	projectRoot         string
	projectID           string
	rootMode            string
	uploadMode          string
	manifestFile        string
	includeFiles        []string
	gitIgnore           bool
	engine              string
	engineExplicit      bool
	outDir              string
	timeout             time.Duration
	interaction         string
	synctex             bool
	haltOnError         bool
	fileLineError       bool
	shellEscape         bool
	jobName             string
	force               bool
	quiet               bool
	jsonOutput          bool
	dryRun              bool
	detach              bool
	watch               bool
	realtime            bool
	controlFiles        []string
	watchInterval       time.Duration
	watchDebounce       time.Duration
	watchMaxWait        time.Duration
	insecure            bool
	entry               string
	exclude             []string
	configPath          string
}

func main() {
	os.Exit(run(os.Args))
}

type compileInvocation struct {
	engine   string
	listOnly bool
	watch    bool
}

func run(args []string) int {
	if len(args) == 0 {
		return 2
	}
	invokedAs := strings.ToLower(filepath.Base(args[0]))
	argv := args[1:]
	defaults := compileInvocation{}
	if len(argv) > 0 {
		switch argv[0] {
		case "version", "--version", "-version":
			terminalPrintf("latexmk %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
			return 0
		case "help", "--help", "-h":
			usage()
			return 0
		case "init":
			return runInit(argv[1:])
		case "meta":
			return runMeta(argv[1:])
		case "doctor":
			return runDoctor(argv[1:])
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
		case "pack":
			return runPack(argv[1:])
		case "resources":
			return runResources(argv[1:])
		case "name":
			return runName(argv[1:])
		case "files":
			defaults.listOnly = true
			argv = argv[1:]
		case "watch":
			defaults.watch = true
			argv = argv[1:]
		case "compile":
			argv = argv[1:]
		}
	}

	// These executable aliases are public CLI spellings, not an engine allowlist.
	if !defaults.listOnly {
		switch invokedAs {
		case "xelatex", "xelatex.exe":
			defaults.engine = "xelatex"
		case "lualatex", "lualatex.exe":
			defaults.engine = "lualatex"
		case "pdflatex", "pdflatex.exe":
			defaults.engine = "pdflatex"
		}
	}
	invocation := newNameInvocation()
	for {
		code := runCompileWithInvocation(argv, defaults, invocation)
		if code != reloadLiveSettings {
			return code
		}
	}
}

func runCompileWithInvocation(args []string, defaults compileInvocation, invocation *nameInvocation) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	changes := compileArguments()
	cfg, parsed, err := loadCommand("compile", cwd, args, changes.Flags()...)
	detachedJSON := parsed != nil && parsed.Bool("json") && parsed.Bool("detach")
	if err != nil {
		return failAgentArguments("compile.start", detachedJSON, err)
	}
	opts := optionsFromConfig(cfg, defaults.listOnly)
	opts.invocation = invocation
	opts.watch = defaults.watch
	if defaults.engine != "" {
		opts.engine = defaults.engine
		opts.engineExplicit = true
	}
	if err := applyCompileArguments(changes, &opts, parsed); err != nil {
		if detachedJSON {
			return failAgentArguments("compile.start", true, err)
		}
		terminalFprintln(os.Stderr, "latexmk:", err)
		terminalFprintln(os.Stderr, "run 'latexmk help' for usage")
		return 2
	}
	if defaults.listOnly {
		opts.dryRun = true
	}
	if opts.target == "" && opts.entry == "" {
		opts.target, err = implicitTarget(cfg)
		if err != nil {
			return failAgentArguments("compile.start", opts.detach && opts.jsonOutput, err)
		}
	}
	if opts.target == "all" {
		return runAllTargets(cwd, cfg, opts, parsed)
	}
	if opts.target != "" {
		if err := applyBuildTarget(&opts, cfg, parsed); err != nil {
			return fail(err)
		}
	}
	return runResolvedCompile(cwd, cfg, opts)
}

// runResolvedCompile executes one configured target without parsing or replaying argv.
func runResolvedCompile(cwd string, cfg config.Resolved, opts compileOptions) int {
	if opts.entry == "" {
		err := errors.New("no TeX entry file was provided")
		if opts.detach {
			return failAgentArguments("compile.start", opts.jsonOutput, err)
		}
		return fail(err)
	}
	if opts.detach && (opts.watch || opts.dryRun) {
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
	capturedResources, captureErr := cliresources.Capture(
		context.Background(),
		opts.projectRoot,
		opts.resourceSpecs,
		opts.resourceIDs,
		opts.denyFiles...)
	if captureErr != nil {
		return fail(captureErr)
	}
	defer func() { _ = capturedResources.Close() }()
	opts.resourceCapture = capturedResources
	opts.denyFiles = append(opts.denyFiles, capturedResources.DenyPaths...)
	lockPath := filepath.Join(opts.projectRoot, cliresources.LockName(opts.resourceOptions.LockFile))
	opts.denyFiles = append(opts.denyFiles, lockPath, lockPath+".guard")
	if opts.detach && (opts.pdfTemplate != "" || opts.pdfKeepLast != nil) {
		return failAgentArguments("compile.start", opts.jsonOutput, errors.New("--detach cannot export a PDF template"))
	}
	if err := preparePDFNaming(&opts); err != nil {
		return failAgentArguments("compile.start", opts.detach && opts.jsonOutput, err)
	}
	if opts.naming != nil {
		defer opts.naming.previewLease.Close()
	}
	if opts.dryRun {
		return printManifest(opts)
	}

	if err := cfg.Authenticate(opts.projectRoot); err != nil {
		return fail(err)
	}
	opts.token = cfg.Token
	opts.controlFiles = append(
		[]string{
			cfg.ConfigPath,
			cfg.UserConfigPath,
			cfg.EnvPath,
			filepath.Join(opts.projectRoot, config.FileName),
			filepath.Join(opts.projectRoot, config.EnvFileName),
		},
		cfg.DenyFiles...)
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
			terminalFprintln(
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
	c.GeneratedFiles = generatedFiles(opts)
	c.ResourceFiles = capturedResources.MountFiles()
	request := requestFromOptions(opts)
	prepCtx, prepCancel := context.WithTimeout(context.Background(), resourcePrepareTimeout(opts))
	meta, prepErr := c.Metadata(prepCtx)
	if prepErr == nil {
		if needsResourceEnvironment(opts) && !meta.Capabilities.ResourceEnvironments {
			prepErr = &client.CapabilityError{Capability: "resource environments"}
		}
		if meta.Capabilities.ResourceEnvironments {
			var environment sharedresources.Environment
			if needsResourceEnvironment(opts) {
				environment, prepErr = prepareResourceEnvironment(prepCtx, c, opts, meta, capturedResources)
			} else {
				// Empty default environments bind an exact runtime without creating a lock
				// in projects that have not opted into managed resources.
				environment, prepErr = c.ResolveEnvironment(prepCtx, sharedresources.ResolveRequest{})
				if prepErr == nil && !supportsEnvironmentEngine(environment, opts.engine) {
					prepErr = fmt.Errorf(
						"environment %q does not support engine %q",
						environment.Baseline.ID,
						opts.engine,
					)
				}
				if prepErr == nil {
					environment, prepErr = c.PrepareEnvironment(prepCtx, environment, nil)
				}
			}
			if prepErr == nil {
				request.EnvironmentDigest = environment.Digest
				c.CacheEnvironment = environment.Digest
				c.SetBaseline(environment.Baseline)
				c.ResourceFiles, prepErr = environmentResourceFiles(environment, capturedResources)
			}
		}
	}
	prepCancel()
	if prepErr != nil {
		return fail(prepErr)
	}
	opts.controlFiles = append(opts.controlFiles, resourceControlPaths(opts)...)

	if opts.realtime {
		return runLive(c, request, opts)
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
		terminalFprintln(os.Stderr, "latexmk: warning:", terminalText(warning))
	}
	terminalPrintf(
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
	var err error
	opts, err = prepareBuildNaming(ctx, opts)
	if err != nil {
		return client.CompileOutput{}, err
	}
	defer releaseBuildNaming(opts)
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
		terminalFprintln(os.Stderr, "latexmk: warning:", terminalText(warning))
	}
	if opts.jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(out.Result)
	} else {
		if !opts.quiet && len(out.Stdout) > 0 {
			terminalFprintf(os.Stdout, "%s", string(out.Stdout))
		}
		if len(out.Stderr) > 0 {
			terminalFprintf(os.Stderr, "%s", string(out.Stderr))
		}
		if cache := out.Result.CompileCache; cache != nil {
			terminalFprintf(
				os.Stderr,
				"latexmk: compile cache=%s restored=%d stored=%d reason=%s\n",
				cache.Status,
				cache.RestoredFiles,
				cache.StoredFiles,
				terminalText(cache.Reason),
			)
			if cache.Warning != "" {
				terminalFprintln(os.Stderr, "latexmk: compile cache:", terminalText(cache.Warning))
			}
		}
		terminalFprintf(
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
			terminalFprintln(os.Stderr, "latexmk: warning: server truncated compiler output")
		}
	}
	if !out.Result.Success {
		if out.Result.Error != "" {
			terminalFprintln(os.Stderr, "latexmk:", terminalText(out.Result.Error))
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
	terminalFprintln(os.Stderr, "latexmk: --realtime is recommended for continuous previews and coalesced compilation")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	files, _, err := c.Manifest(request.Entry, request.Engine)
	if err != nil {
		return fail(fmt.Errorf("initialize watch manifest: %w", err))
	}
	terminalFprintf(
		os.Stderr,
		"latexmk: watching %d selected files (interval=%s debounce=%s maxWait=%s)\n",
		len(files),
		opts.watchInterval,
		opts.watchDebounce,
		opts.watchMaxWait,
	)
	for {
		refreshed, _, refreshErr := c.Manifest(request.Entry, request.Engine)
		if refreshErr != nil {
			terminalFprintln(
				os.Stderr,
				"latexmk: warning: could not refresh manifest before watch compile:",
				refreshErr,
			)
		} else {
			files = refreshed
		}
		before := files
		out, compileErr := compileWithTimeout(ctx, c, request, opts)
		if ctx.Err() != nil {
			terminalFprintln(os.Stderr, "latexmk: watch stopped")
			return 0
		}
		code := reportCompile(out, compileErr, opts)
		if code != 0 {
			terminalFprintf(
				os.Stderr,
				"latexmk: watch compile failed with status %d; waiting for another change\n",
				code,
			)
		}

		after, _, manifestErr := c.Manifest(request.Entry, request.Engine)
		if manifestErr != nil {
			terminalFprintln(os.Stderr, "latexmk: warning: could not refresh watch manifest:", manifestErr)
			after = before
		}
		if selectedFilesChanged(before, after) {
			files = after
			terminalFprintln(
				os.Stderr,
				"latexmk: selected files changed during compilation; scheduling another immutable compile",
			)
			if !waitForContext(ctx, opts.watchDebounce) {
				terminalFprintln(os.Stderr, "latexmk: watch stopped")
				return 0
			}
			continue
		}
		files = after
		tracker, trackErr := newSourceTracker(c, request, opts, files, nil)
		if trackErr != nil {
			return fail(trackErr)
		}
		changed, waitErr := tracker.Wait(ctx)
		if waitErr != nil {
			if ctx.Err() != nil {
				terminalFprintln(os.Stderr, "latexmk: watch stopped")
				return 0
			}
			return fail(waitErr)
		}
		for _, name := range changed {
			if strings.HasPrefix(name, "settings resources: ") {
				return reloadLiveSettings
			}
		}
		terminalFprintln(os.Stderr, "latexmk: change detected:", strings.Join(changed, ", "))
	}
}

func compileArguments() *argv.Options[compileOptions] {
	options := &argv.Options[compileOptions]{}
	options.Value("resource", func(o *compileOptions, v string) error {
		if !sharedresources.ValidID(v) {
			return errors.New("--resource requires a resource ID")
		}
		o.resourceAddIDs = append(o.resourceAddIDs, v)
		return nil
	})
	options.Value("without-resource", func(o *compileOptions, v string) error {
		if !sharedresources.ValidID(v) {
			return errors.New("--without-resource requires a resource ID")
		}
		o.resourceRemoveIDs = append(o.resourceRemoveIDs, v)
		return nil
	})
	options.Bool("no-resources", func(o *compileOptions, v bool) error {
		if v {
			o.resourceIDs = []string{}
			o.resourceAddIDs = nil
			o.resourceExplicit = true
		}
		return nil
	})
	options.Value("environment", func(o *compileOptions, v string) error {
		if !sharedresources.ValidID(v) {
			return errors.New("--environment requires a registered baseline ID")
		}
		o.environment = v
		o.environmentExplicit = true
		return nil
	})
	options.Value("lockfile", func(o *compileOptions, v string) error {
		if v == "" {
			return errors.New("--lockfile requires a path")
		}
		o.resourceOptions.LockFile = v
		return nil
	})
	options.Bool("frozen-lockfile", func(o *compileOptions, v bool) error {
		o.resourceOptions.LockMode = "auto"
		if v {
			o.resourceOptions.LockMode = "frozen"
		}
		return nil
	})
	options.Value("prepare-timeout", func(o *compileOptions, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return errors.New("--prepare-timeout must be positive")
		}
		o.resourceOptions.PrepareTimeout = v
		return nil
	})
	options.Value("pdf-template", func(o *compileOptions, v string) error {
		if v == "" {
			return errors.New("--pdf-template requires a nonempty value")
		}
		o.pdfTemplate, o.pdfExport = v, ""
		return nil
	})
	options.Value("name-scope", func(o *compileOptions, v string) error {
		if v != "invocation" && v != "build" {
			return errors.New("--name-scope must be invocation or build")
		}
		o.nameScope = v
		return nil
	})
	options.Value("name-timezone", func(o *compileOptions, v string) error {
		if v == "" {
			return errors.New("--name-timezone requires a nonempty value")
		}
		o.nameTimeZone = v
		return nil
	})
	options.Value("pdf-keep-last", func(o *compileOptions, v string) error {
		o.pdfKeepLast = nil
		if v != "all" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return errors.New("--pdf-keep-last must be a positive integer or all")
			}
			o.pdfKeepLast = &n
		}
		return nil
	})
	options.Value("server", func(o *compileOptions, v string) error { o.server = v; return nil })
	options.Value("ignore-file", func(o *compileOptions, v string) error {
		if o.ignoreFiles == nil {
			o.ignoreFiles = []string{}
		}
		o.ignoreFiles = append(o.ignoreFiles, v)
		return nil
	})
	options.Bool("no-ignore-files", func(o *compileOptions, v bool) error {
		if v {
			o.ignoreFiles = []string{}
		}
		return nil
	})
	options.Value("explain", func(o *compileOptions, v string) error { o.explain = v; o.dryRun = true; return nil })
	options.Value("unmatched-glob", func(o *compileOptions, v string) error {
		if v != "error" && v != "warn" && v != "ignore" {
			return errors.New("--unmatched-glob must be error, warn, or ignore")
		}
		o.unmatchedGlob = v
		return nil
	})
	options.Value("local-cache", func(o *compileOptions, v string) error {
		if v != "none" && v != "cache" && v != "output" {
			return errors.New("--local-cache must be none, cache, or output")
		}
		o.auxiliary.Local = v
		return nil
	})
	options.Value("server-cache-ttl", func(o *compileOptions, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return errors.New("--server-cache-ttl must be a positive duration")
		}
		o.auxiliary.ServerTTL = v
		return nil
	})
	options.Value("target", func(o *compileOptions, v string) error {
		if v == "" {
			return errors.New("--target requires a nonempty name")
		}
		o.target = v
		return nil
	})
	options.Value("project-root", func(o *compileOptions, v string) error { o.projectRoot = v; return nil })
	options.Value("project-id", func(o *compileOptions, v string) error { o.projectID = v; return nil })
	options.Value("root-mode", func(o *compileOptions, v string) error {
		if v != "entry" && v != "git" {
			return fmt.Errorf("--root-mode must be entry or git, got %q", v)
		}
		o.rootMode = v
		return nil
	})
	options.Value("upload-mode", func(o *compileOptions, v string) error {
		if v == "ignore" {
			v = "all"
		}
		if v != "auto" && v != "manifest" && v != "all" {
			return fmt.Errorf("--upload-mode must be auto, manifest, or all, got %q", v)
		}
		o.uploadMode = v
		return nil
	})
	options.Value("server-cache", func(o *compileOptions, v string) error {
		if v != "none" && v != "reuse" && v != "retain" {
			return errors.New("--server-cache must be none, retain, or reuse")
		}
		o.auxiliary.Server = v
		return nil
	})
	options.Value("manifest", func(o *compileOptions, v string) error { o.manifestFile = v; return nil })
	options.Value(
		"include-file",
		func(o *compileOptions, v string) error { o.includeFiles = append(o.includeFiles, v); return nil },
	)
	options.Bool("gitignore", func(o *compileOptions, v bool) error { o.gitIgnore = v; return nil })
	options.Bool("no-gitignore", func(o *compileOptions, v bool) error { o.gitIgnore = !v; return nil })
	options.Value("out-dir", func(o *compileOptions, v string) error { o.outDir = v; return nil }, "output-directory")
	options.Value(
		"engine",
		func(o *compileOptions, v string) error { o.engine = v; o.engineExplicit = true; return nil },
	)
	options.Value(
		"timeout",
		func(o *compileOptions, v string) error { d, err := time.ParseDuration(v); o.timeout = d; return err },
	)
	options.Value("interaction", func(o *compileOptions, v string) error { o.interaction = v; return nil })
	options.Value("jobname", func(o *compileOptions, v string) error { o.jobName = v; return nil })
	options.Bool("synctex", func(o *compileOptions, v bool) error { o.synctex = v; return nil })
	options.Bool("no-synctex", func(o *compileOptions, v bool) error { o.synctex = !v; return nil })
	options.Bool("shell-escape", func(o *compileOptions, v bool) error { o.shellEscape = v; return nil })
	options.Bool("no-shell-escape", func(o *compileOptions, v bool) error { o.shellEscape = !v; return nil })
	options.Bool("halt-on-error", func(o *compileOptions, v bool) error { o.haltOnError = v; return nil })
	options.Bool("no-halt-on-error", func(o *compileOptions, v bool) error { o.haltOnError = !v; return nil })
	options.Bool("file-line-error", func(o *compileOptions, v bool) error { o.fileLineError = v; return nil })
	options.Bool("no-file-line-error", func(o *compileOptions, v bool) error { o.fileLineError = !v; return nil })
	options.Bool("xelatex", func(o *compileOptions, v bool) error {
		if v {
			o.engine = "xelatex"
			o.engineExplicit = true
		}
		return nil
	}, "pdfxe")
	options.Bool("lualatex", func(o *compileOptions, v bool) error {
		if v {
			o.engine = "lualatex"
			o.engineExplicit = true
		}
		return nil
	}, "pdflua")
	options.Bool("pdf", func(o *compileOptions, v bool) error {
		if v {
			o.engine = "pdflatex"
			o.engineExplicit = true
		}
		return nil
	}, "pdflatex")
	options.Bool("force", func(o *compileOptions, v bool) error { o.force = v; return nil }, "g", "gg")
	options.Bool("quiet", func(o *compileOptions, v bool) error { o.quiet = v; return nil }, "silent")
	options.Bool("json", func(o *compileOptions, v bool) error { o.jsonOutput = v; return nil })
	options.Bool("dry-run", func(o *compileOptions, v bool) error { o.dryRun = v; return nil })
	options.Bool("detach", func(o *compileOptions, v bool) error { o.detach = v; return nil })
	options.Bool("realtime", func(o *compileOptions, v bool) error { o.realtime = v; return nil })
	options.Bool("watch", func(o *compileOptions, v bool) error { o.watch = v; return nil })
	options.Value("watch-interval", func(o *compileOptions, v string) error {
		d, err := time.ParseDuration(v)
		o.watchInterval = d
		return err
	})
	options.Value("watch-debounce", func(o *compileOptions, v string) error {
		d, err := time.ParseDuration(v)
		o.watchDebounce = d
		return err
	})
	options.Bool("insecure-skip-verify", func(o *compileOptions, v bool) error { o.insecure = v; return nil })
	options.Value("watch-max-wait", func(o *compileOptions, v string) error {
		d, err := time.ParseDuration(v)
		o.watchMaxWait = d
		return err
	})
	options.Bool("version", func(_ *compileOptions, _ bool) error {
		return errors.New("--version must be used without a compile target")
	})
	return options
}

func applyCompileArguments(changes *argv.Options[compileOptions], opts *compileOptions, parsed *cli.Command) error {
	if err := changes.Apply(opts); err != nil {
		return err
	}
	positionals := parsed.Args().Slice()
	if len(positionals) > 1 {
		return fmt.Errorf("only one entry file is supported, got %d", len(positionals))
	}
	if len(positionals) == 1 {
		opts.entry = positionals[0]
	}
	opts.watch = opts.watch || opts.realtime
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if opts.watch && opts.watchInterval <= 0 {
		return errors.New("watch interval must be positive")
	}
	if opts.watch && opts.watchDebounce < 0 {
		return errors.New("watch debounce cannot be negative")
	}
	if opts.watch && opts.watchMaxWait <= 0 {
		return errors.New("watch max wait must be positive")
	}
	return config.ValidateNaming(config.NamingConfig{Scope: opts.nameScope, TimeZone: opts.nameTimeZone})
}

func normalizeCompilePaths(opts *compileOptions, cwd string) error {
	finalizeResourceOptions(opts)
	if err := validateResourceCLI(*opts); err != nil {
		return err
	}
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
		if opts.generated != nil {
			deny = append(deny, opts.generated.snapshot()...)
		}
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
			terminalPrintf("%s: %s\n", explanation.Path, explanation.Reason)
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
	terminalPrintf(
		"project root: %s\nentry: %s\nupload mode: %s\nresolved: %t\nfiles: %d\nbytes: %d\n",
		opts.projectRoot,
		opts.entry,
		opts.uploadMode,
		result.Resolved,
		result.Stats.Files,
		result.Stats.Bytes,
	)
	for _, file := range result.Files {
		terminalPrintf("%10d  %s  %s  (%s)\n", file.Size, file.SHA256, file.Path, file.Reason)
	}
	for _, diagnostic := range result.Diagnostics {
		terminalFprintf(os.Stderr, "latexmk: dependency: %s\n", dependency.FormatDiagnostic(diagnostic))
	}
	if !result.Resolved {
		terminalFprintln(
			os.Stderr,
			"latexmk: dependency discovery has unresolved references; fix them or review --upload-mode all",
		)
		return 1
	}
	return 0
}

type metadataOptions struct {
	server               string
	timeout              time.Duration
	insecure, jsonOutput bool
}

func metadataArguments() *argv.Options[metadataOptions] {
	changes := &argv.Options[metadataOptions]{}
	changes.Value("server", func(o *metadataOptions, v string) error { o.server = v; return nil })
	changes.Value(
		"timeout",
		func(o *metadataOptions, v string) error { d, err := time.ParseDuration(v); o.timeout = d; return err },
	)
	changes.Bool("insecure-skip-verify", func(o *metadataOptions, v bool) error { o.insecure = v; return nil })
	changes.Bool("json", func(o *metadataOptions, v bool) error { o.jsonOutput = v; return nil })
	return changes
}

func runMeta(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	changes := metadataArguments()
	cfg, parsed, err := loadCommand("meta", cwd, args, changes.Flags()...)
	if err != nil {
		return fail(err)
	}
	opts := metadataOptions{server: cfg.Server, timeout: cfg.Timeout, insecure: cfg.InsecureSkipVerify}
	if err := changes.Apply(&opts); err != nil {
		return fail(err)
	}
	if parsed.Args().Len() != 0 {
		return fail(errors.New("metadata commands do not accept positional arguments"))
	}
	server, timeout, insecure, jsonOutput := opts.server, opts.timeout, opts.insecure, opts.jsonOutput
	if err := cfg.Authenticate(cfg.ProjectRoot); err != nil {
		return fail(err)
	}
	c, err := client.New(server, cfg.Token, timeout, insecure)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	meta, err := c.Metadata(ctx)
	if err != nil {
		return fail(err)
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(meta)
		return 0
	}
	printMetadata(meta)
	return 0
}

func printMetadata(meta protocol.Metadata) {
	terminalPrintf(
		"service: %s %s\nprotocol: %d\nprofile: %s\nauth: %s\ndatabase: %s\nengines: %s\n",
		meta.Service,
		meta.Version,
		meta.ProtocolVersion,
		meta.ImageProfile,
		meta.AuthMode,
		meta.Database,
		strings.Join(meta.Capabilities.Engines, ", "),
	)
	names := make([]string, 0, len(meta.Toolchain))
	for name := range meta.Toolchain {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v := meta.Toolchain[name]; v != "" {
			terminalPrintf("%s: %s\n", name, v)
		}
	}
}

func runInit(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	changes := &argv.Options[string]{}
	changes.Value("server", func(server *string, value string) error { *server = value; return nil })
	parsed, err := argv.Parse("init", args, changes.Flags()...)
	if err != nil {
		return fail(err)
	}
	if parsed.Args().Len() != 0 {
		return fail(errors.New("init does not accept positional arguments"))
	}
	server := "http://127.0.0.1:8080"
	if err := changes.Apply(&server); err != nil {
		return fail(err)
	}
	path := filepath.Join(cwd, config.FileName)
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
	terminalPrintln(path)
	terminalFprintln(
		os.Stderr,
		"latexmk: recommended: run 'latexmk cache ignore' to protect the local project identity",
	)
	terminalFprintln(
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
		terminalFprintf(os.Stderr, "latexmk: doctor: could not inspect project cache Git policy: %v\n", err)
		return
	}
	status, err := client.InspectProjectCacheGitIgnore(root)
	if err != nil {
		terminalFprintf(os.Stderr, "latexmk: doctor: could not inspect project cache Git policy: %v\n", err)
		return
	}
	if !status.InWorkTree {
		if !jsonOutput {
			terminalPrintln("project cache Git ignore: not applicable (not a Git work tree)")
		}
		return
	}
	if status.Ignored {
		if !jsonOutput {
			terminalPrintln("project cache Git ignore: configured")
		}
		return
	}
	terminalFprintln(os.Stderr, "latexmk: doctor: "+client.ProjectCacheGitAdvice)
}

func runClean(args []string) int {
	parsed, err := argv.Parse("clean", args)
	if err != nil {
		return fail(err)
	}
	entries := parsed.Args().Slice()
	if len(entries) > 1 {
		return fail(errors.New("clean accepts at most one entry file"))
	}
	entry := "main.tex"
	if len(entries) == 1 {
		entry = entries[0]
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
	terminalPrintf("removed %d generated files\n", removed)
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

func remoteCleanArguments() *argv.Options[remoteCleanOptions] {
	changes := &argv.Options[remoteCleanOptions]{}
	changes.Value("server", func(o *remoteCleanOptions, v string) error { o.server = v; return nil })
	changes.Bool("insecure-skip-verify", func(o *remoteCleanOptions, v bool) error { o.insecure = v; return nil })
	changes.Value("timeout", func(o *remoteCleanOptions, v string) error {
		d, err := time.ParseDuration(v)
		o.timeout = d
		return err
	})
	changes.Value("project-root", func(o *remoteCleanOptions, v string) error { o.projectRoot = v; return nil })
	changes.Value("project-id", func(o *remoteCleanOptions, v string) error { o.projectID = v; return nil })
	changes.Bool("legacy-project-id", func(o *remoteCleanOptions, v bool) error { o.legacyProjectID = v; return nil })
	changes.Value("scope", func(o *remoteCleanOptions, v string) error { o.scope = v; return nil })
	changes.Value("plan-id", func(o *remoteCleanOptions, v string) error { o.planID = v; return nil })
	changes.Bool("yes", func(o *remoteCleanOptions, v bool) error { o.yes = v; return nil })
	changes.Bool("dry-run", func(o *remoteCleanOptions, v bool) error { o.dryRun = v; return nil })
	changes.Bool("json", func(o *remoteCleanOptions, v bool) error { o.jsonOutput = v; return nil })
	return changes
}

func validateRemoteCleanOptions(opts remoteCleanOptions) error {
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
	changes := remoteCleanArguments()
	cfg, parsed, err := loadCommand("remote-clean", cwd, args, changes.Flags()...)
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
	if err := changes.Apply(&opts); err != nil {
		return fail(err)
	}
	if parsed.Args().Len() != 0 {
		return fail(errors.New("remote clean does not accept positional arguments"))
	}
	if err := validateRemoteCleanOptions(opts); err != nil {
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
		terminalPrintf(
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
	terminalPrintf("plan ID: %s\n", plan.ID)
	writeRemoteCleanupReport(report)
	terminalPrintf("reclaimed bytes: %d\n", report.ReclaimedBytes)
	return 0
}

func writeRemoteCleanupReport(report protocol.CleanupReport) {
	if report.Scope == "cache" || report.Scope == "project" {
		terminalPrintf("compile caches: %d (%d bytes)\n", report.CompileCaches, report.CompileCacheBytes)
	}
	terminalPrintf("project ID: %s\nscope: %s\ndry run: %t\n", report.ProjectID, report.Scope, report.DryRun)
	if report.Scope == "snapshot" || report.Scope == "project" {
		terminalPrintf(
			"snapshot: %t (%d files, %d bytes)\n",
			report.SnapshotPresent,
			report.SnapshotFiles,
			report.SnapshotBytes,
		)
	}
	if report.Scope == "results" || report.Scope == "project" {
		terminalPrintf("results: %d (%d bytes)\n", report.Results, report.ResultBytes)
	}
	if report.Scope == "project" {
		terminalPrintf("terminal jobs: %d\n", report.Jobs)
	}
	if len(report.ActiveJobs) > 0 {
		terminalPrintf("active jobs (not deleted): %s\n", strings.Join(report.ActiveJobs, ", "))
	}
}

func usage() {
	terminalPrintln("Resource commands: latexmk resources lock|fetch|inspect [--target NAME] [--json]")
	terminalPrintln(
		"Resource flags: --resource ID, --without-resource ID, --no-resources, --environment ID, --lockfile PATH, --frozen-lockfile, --prepare-timeout DURATION",
	)
	terminalPrint(`latexmk - remote, PaaS-hosted LaTeX compiler

Usage:
  latexmk compile [options] [main.tex]
  latexmk watch [options] [main.tex]
  latexmk [latex-compatible-options] [main.tex]
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
  latexmk files [options] [main.tex]
  latexmk pack [options] [main.tex] --mode default|arxiv [--output FILE.zip] [--verify]
  latexmk pack [options] [main.tex] --output-template TEMPLATE.zip
  latexmk name render --template TEMPLATE [main.tex|--target NAME] [--json]
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
  --pdf-template TEMPLATE      Export a PDF using {variable[:format]} placeholders
  --name-scope MODE            invocation (default) or build; controls template refresh
  --name-timezone ZONE         local (default), UTC, or an IANA time zone
  --pdf-keep-last N|all        Retain N distinct PDF export paths; default keeps all
  --gitignore                  Respect Git ignore rules (default)
  --no-gitignore               Include Git-ignored files unless otherwise excluded
  --out-dir DIR                Local root for returned artifacts
  --engine NAME                Registered engine name (default xelatex)
  --timeout 3m                 End-to-end request timeout
  --shell-escape               Request shell escape; server policy may reject it
  --jobname NAME               TeX job name
  --no-synctex                 Disable SyncTeX
  --json                       Print machine-readable result
  --dry-run                    Print the upload manifest without contacting the server
  --detach                     Return after creating an immutable queued job
  --realtime                   Watch with revisioned sessions and atomic PDF bundles
  --watch                      Ordinary jobs after changes; prefer --realtime
  --watch-interval 500ms       Refresh selection and poll selected files
  --watch-debounce 500ms       Wait for rapid edits to settle before compiling
  --watch-max-wait 2.5s        Compile after this bound even during continuous edits

The executable may be symlinked as xelatex, lualatex, or pdflatex.
Configuration is read from user/project JSON, .env.latexmk, environment and CLI flags.
Without an entry or --target, the configured defaultTarget or sole target is used.

Remote cleanup previews create a ten-minute local plan. Apply that exact plan
with --plan-id PLAN_ID --yes. Use --legacy-project-id only for data created by
older path-derived project IDs.
`)
}

func fail(err error) int {
	terminalFprintln(os.Stderr, "latexmk:", err)
	return 2
}
