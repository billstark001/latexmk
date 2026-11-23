package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/exporthistory"
	"github.com/billstark001/latexmk/packages/cli/internal/naming"
	projectpack "github.com/billstark001/latexmk/packages/cli/internal/pack"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/argv"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

type packOptions struct {
	mode, output                string
	outputTemplate              string
	outputFromPublication       bool
	verifyMode                  string
	modeExplicit                bool
	publication                 string
	verifyIn                    string
	resourceActions             map[string]string
	includeFiles                []string
	exclude                     []string
	explain                     bool
	preservePermissions         bool
	preservePermissionsExplicit bool
}

type packView struct {
	PreservePermissions bool                   `json:"preservePermissions"`
	Mode                string                 `json:"mode"`
	Output              string                 `json:"output"`
	Entry               string                 `json:"entry"`
	Engine              string                 `json:"engine"`
	Files               []projectarchive.File  `json:"files"`
	RequiresBuild       bool                   `json:"requiresBuild"`
	Verified            bool                   `json:"verified"`
	BuildJobID          string                 `json:"buildJobId,omitempty"`
	Warnings            []string               `json:"warnings,omitempty"`
	Publication         string                 `json:"publication,omitempty"`
	Plan                *projectpack.Plan      `json:"plan,omitempty"`
	Verification        resources.Verification `json:"verification"`
}

func packArguments() *argv.Options[packOptions] {
	changes := new(argv.Options[packOptions])
	value := func(name string, apply func(*packOptions, string)) {
		changes.Value(name, func(options *packOptions, value string) error {
			if value == "" {
				return fmt.Errorf("--%s requires a nonempty value", name)
			}
			apply(options, value)
			return nil
		})
	}
	value("mode", func(options *packOptions, value string) { options.mode, options.modeExplicit = value, true })
	value("output", func(options *packOptions, value string) { options.output = value })
	value("output-template", func(options *packOptions, value string) { options.outputTemplate = value })
	value("publication", func(options *packOptions, value string) { options.publication = value })
	value(
		"verify-in",
		func(options *packOptions, value string) { options.verifyIn, options.verifyMode = value, "target" },
	)
	value(
		"pack-include",
		func(options *packOptions, value string) { options.includeFiles = append(options.includeFiles, value) },
	)
	value("pack-exclude", func(options *packOptions, value string) { options.exclude = append(options.exclude, value) })
	changes.Value("publish-resource", func(options *packOptions, value string) error {
		id, action, ok := strings.Cut(value, "=")
		if !ok || !resources.ValidID(id) || (action != "bundle" && action != "provided" && action != "omit") {
			return errors.New("--publish-resource requires ID=bundle|provided|omit")
		}
		if options.resourceActions == nil {
			options.resourceActions = make(map[string]string)
		}
		options.resourceActions[id] = action
		return nil
	})
	changes.Bool("preserve-permissions", func(options *packOptions, value bool) error {
		options.preservePermissions, options.preservePermissionsExplicit = value, true
		return nil
	})
	changes.Bool("explain", func(options *packOptions, value bool) error { options.explain = value; return nil })
	changes.Bool("verify", func(options *packOptions, value bool) error {
		if options.verifyIn == "" {
			options.verifyMode = ""
			if value {
				options.verifyMode = "build"
			}
		}
		return nil
	})
	return changes
}

func packCompileFlags(changes *argv.Options[compileOptions]) []cli.Flag {
	var flags []cli.Flag
	for _, flag := range changes.Flags() {
		if !slices.Contains(flag.Names(), "explain") {
			flags = append(flags, flag)
		}
	}
	return flags
}

func validatePackArguments(options packOptions, parsed *cli.Command) error {
	for _, name := range []string{
		"local-cache", "server-cache", "server-cache-ttl", "force", "synctex", "no-synctex",
	} {
		if parsed.IsSet(name) {
			return fmt.Errorf("--%s is not a pack option; pack verification uses a cold build", name)
		}
	}
	if options.output != "" && options.outputTemplate != "" {
		return errors.New("--output and --output-template are mutually exclusive")
	}
	return projectpack.ValidateMode(options.mode)
}

func runPack(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent("pack", false, err)
	}
	compileChanges := compileArguments()
	packChanges := packArguments()
	flags := append(packCompileFlags(compileChanges), packChanges.Flags()...)
	cfg, parsed, err := loadCommand("pack", cwd, args, flags...)
	jsonOutput := parsed != nil && parsed.Bool("json")
	fail := func(err error) int { return failAgent("pack", jsonOutput, err) }
	failArgs := func(err error) int { return failAgentArguments("pack", jsonOutput, err) }
	if err != nil {
		return failArgs(err)
	}
	packing := packOptions{mode: "default"}
	if err := packChanges.Apply(&packing); err != nil {
		return failArgs(err)
	}
	if err := validatePackArguments(packing, parsed); err != nil {
		return failArgs(err)
	}
	opts := optionsFromConfig(cfg, false)
	opts.invocation = newNameInvocation()
	if err := applyCompileArguments(compileChanges, &opts, parsed); err != nil {
		return failArgs(err)
	}
	if err := applyPublication(&packing, &opts, cfg); err != nil {
		return failArgs(err)
	}
	if err := projectpack.ValidateMode(packing.mode); err != nil {
		return failArgs(err)
	}
	if packing.explain {
		opts.dryRun = true
	}
	if opts.watch || opts.detach || opts.target == "all" || opts.explain != "" {
		return failArgs(errors.New("pack supports one entry/target and cannot watch, detach, or explain"))
	}
	if parsed.IsSet("pdf-template") || parsed.IsSet("pdf-keep-last") {
		return failArgs(errors.New("pack does not export PDFs; use --output-template for the ZIP name"))
	}
	if opts.target == "" && opts.entry == "" {
		opts.target, err = implicitTarget(cfg)
		if err != nil {
			return failArgs(err)
		}
	}
	if opts.target != "" {
		if err := applyBuildTarget(&opts, cfg, parsed); err != nil {
			return failArgs(err)
		}
	}
	if opts.entry == "" {
		return failArgs(errors.New("no TeX entry file was provided"))
	}
	if err := normalizeCompilePaths(&opts, cwd); err != nil {
		return fail(err)
	}
	if packing.outputFromPublication && !filepath.IsAbs(packing.output) {
		packing.output = filepath.Join(opts.projectRoot, packing.output)
	}
	if err := prepareGeneratedPaths(&opts); err != nil {
		return fail(err)
	}
	if packing.outputTemplate != "" {
		template, err := naming.Parse(packing.outputTemplate)
		if err != nil {
			return failArgs(err)
		}
		packing.output, err = renderOutputName(context.Background(), template, opts)
		if err != nil {
			return failArgs(err)
		}
		if packing.output == "" {
			return failArgs(errors.New("pack output template rendered an empty path"))
		}
	}
	stem := strings.TrimSuffix(filepath.Base(opts.entry), filepath.Ext(opts.entry))
	if packing.mode == "arxiv" && (opts.shellEscape || (opts.jobName != "" && opts.jobName != stem)) {
		return failArgs(errors.New("arxiv mode requires shell escape disabled and the entry's default job name"))
	}
	if packing.output == "" {
		packing.output = filepath.Join(opts.outDir, stem+"-"+packing.mode+".zip")
	}
	if err := projectpack.ValidateOutput(packing.output); err != nil {
		return failArgs(err)
	}
	packing.output, err = filepath.Abs(packing.output)
	if err != nil {
		return fail(err)
	}
	packing.output, err = canonicalPackPath(packing.output)
	if err != nil {
		return fail(err)
	}
	if err := projectpack.ValidateOutput(packing.output); err != nil {
		return failArgs(err)
	}
	// An existing output can never become its own input in upload mode all.
	opts.denyFiles = append(opts.denyFiles, packing.output)
	lease := opts.generated.reserve(packing.output)
	defer lease.Close()
	request := requestFromOptions(opts)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	resourceCapture, err := cliresources.Capture(
		ctx,
		opts.projectRoot,
		opts.resourceSpecs,
		opts.resourceIDs,
		opts.denyFiles...)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = resourceCapture.Close() }()
	opts.resourceCapture = resourceCapture
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, resourcePrepareTimeout(opts))
	defer cancelPrepare()
	view := packView{
		PreservePermissions: packing.preservePermissions,
		Mode:                packing.mode,
		Output:              packing.output,
		Entry:               opts.entry,
		Engine:              opts.engine,
		Publication:         packing.publication,
		RequiresBuild:       packing.mode == "arxiv" || packing.verifyMode == "build" || packing.verifyMode == "target",
		Verification:        resources.Verification{Status: "not-requested", Scope: publicationScope(packing)},
	}
	if view.RequiresBuild {
		view.Verification.Status = "pending"
	}
	var connected *client.Client
	var meta protocol.Metadata
	var finalEnvironment resources.Environment
	needsServer := view.RequiresBuild || hasServerBundle(resourceCapture.Resources, packing)
	for _, resource := range resourceCapture.Resources {
		needsServer = needsServer || resourceAction(packing, resource.ID) == "provided"
	}
	if needsServer && !opts.dryRun {
		if err := cfg.Authenticate(opts.projectRoot); err != nil {
			return fail(err)
		}
		connected, err = client.New(opts.server, cfg.Token, 0, opts.insecure)
		if err != nil {
			return fail(err)
		}
		connected.ProjectRoot, connected.ProjectID = opts.projectRoot, opts.projectID
		meta, err = connected.Metadata(prepareCtx)
		if err != nil {
			return fail(err)
		}
		var exports []*projectarchive.Frozen
		defer func() {
			for _, exported := range exports {
				_ = exported.Close()
			}
		}()
		exports, err = exportPublicationResources(prepareCtx, connected, opts, packing, meta, resourceCapture)
		if err != nil {
			return fail(err)
		}
	}
	planner, err := projectpack.NewPlanner(projectpack.PlanOptions{
		Entry:               opts.entry,
		Mode:                packing.mode,
		PreservePermissions: packing.preservePermissions,
		Exclude:             packing.exclude,
		Resources:           resourceCapture.Resources,
		Actions:             packing.resourceActions,
		ResourceFiles:       resourceCapture.MountFilesByID,
		Preview:             opts.dryRun,
	})
	if err != nil {
		return fail(err)
	}
	baseline := packing.verifyIn
	if baseline == "" {
		baseline = opts.environment
	}
	finalRequest := resources.ResolveRequest{
		Baseline: baseline,
		Provided: planner.Provided(),
		Requires: opts.requires,
	}
	managedEnvironment := len(resourceCapture.Resources) != 0 || packing.verifyIn != "" || opts.environment != "" ||
		packing.publication != ""
	if connected != nil && (managedEnvironment || (view.RequiresBuild && meta.Capabilities.ResourceEnvironments)) {
		if managedEnvironment {
			finalEnvironment, err = resolveResourceEnvironment(prepareCtx, connected, opts, meta, resourceCapture,
				publicationPlanKey(packing, opts), finalRequest, resourcePrepare)
		} else {
			// Old, resource-free pack calls gain exact baseline reporting when the
			// server supports it, without creating new project lock files.
			finalEnvironment, err = connected.ResolveEnvironment(prepareCtx, finalRequest)
			if err == nil && !supportsEnvironmentEngine(finalEnvironment, opts.engine) {
				err = fmt.Errorf("environment %q does not support engine %q", finalEnvironment.Baseline.ID, opts.engine)
			}
			if err == nil {
				finalEnvironment, err = connected.PrepareEnvironment(prepareCtx, finalEnvironment, nil)
			}
		}
		if err != nil {
			return fail(err)
		}
		if len(finalEnvironment.Resources) != 0 {
			return fail(errors.New("publication verification environment unexpectedly injects resources"))
		}
		request.EnvironmentDigest = finalEnvironment.Digest
		view.Verification.EnvironmentID = finalEnvironment.Baseline.ID
		view.Verification.EnvironmentDigest = finalEnvironment.Digest
	}
	cancelPrepare()
	// Resolve registered exports and the final baseline before discovery sees
	// filename references. Only frozen source members enter the ZIP.
	c := selectionClient(opts)
	c.BaselineFiles = nil
	c.IncludeFiles = append(c.IncludeFiles, packing.includeFiles...)
	if finalEnvironment.Digest != "" {
		c.SetBaseline(finalEnvironment.Baseline)
	} else if opts.dryRun {
		lock, err := cliresources.LoadLock(opts.projectRoot, opts.resourceOptions.LockFile)
		if err != nil {
			return fail(err)
		}
		normalized, err := resources.NormalizeRequest(finalRequest)
		if err != nil {
			return fail(err)
		}
		specDigest, err := resources.Hash(normalized)
		if err != nil {
			return fail(err)
		}
		if locked, ok := lock.Plans[publicationPlanKey(packing, opts)]; ok && locked.SpecDigest == specDigest {
			c.SetBaseline(locked.Environment.Baseline)
			view.Warnings = append(
				view.Warnings,
				"using the saved baseline inventory for offline discovery; verification is pending",
			)
		}
		warnings, err := addPendingPublicationReferences(c, request, resourceCapture.Resources)
		if err != nil {
			return fail(err)
		}
		view.Warnings = append(view.Warnings, warnings...)
	}
	captured, err := c.FreezeSnapshot(ctx, request, nil, protocol.Metadata{}, nil)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = captured.Close() }()
	view.Warnings = append(view.Warnings, captured.Warnings...)
	plan, err := planner.Build(captured.Files, nil)
	if err != nil {
		return fail(err)
	}
	view.Plan, view.Files = &plan, plan.Files
	if opts.dryRun {
		return reportPack(view, jsonOutput, true)
	}
	files := plan.Files
	if view.RequiresBuild {
		output, err := os.MkdirTemp("", "latexmk-pack-build-")
		if err != nil {
			return fail(err)
		}
		defer func() { _ = os.RemoveAll(output) }()
		// Pack builds are cold and retain auxiliaries only for immediate download.
		request.Force, request.Synctex = true, false
		request.Auxiliary = protocol.AuxiliaryOptions{Local: "output", Server: "none"}
		initial, err := projectarchive.Freeze(ctx, files, projectpack.MaxFiles, projectpack.MaxBytes, nil)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = initial.Close() }()
		build, err := connected.CompileCaptured(ctx, request, initial, output, meta)
		if err != nil {
			return fail(err)
		}
		if !build.Result.Success {
			return fail(packBuildError("pack build", build))
		}
		view.BuildJobID = build.Result.RequestID
		files = initial.Files
		if packing.mode == "arxiv" {
			generated, err := submissionFiles(output, opts, build.Result.Artifacts)
			if err != nil {
				return fail(err)
			}
			plan, err = planner.Build(captured.Files, generated)
			if err != nil {
				return fail(err)
			}
			// Only arXiv adds generated members after the first build. Default
			// verification can publish its already verified initial capture.
			bundle, err := projectarchive.Freeze(ctx, plan.Files, projectpack.MaxFiles, projectpack.MaxBytes, nil)
			if err != nil {
				return fail(err)
			}
			defer func() { _ = bundle.Close() }()
			files = bundle.Files
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
			view.Verification.JobID = verification.Result.RequestID
		} else {
			view.Verification.JobID = build.Result.RequestID
		}
		view.Verified = true
		view.Verification.Status = "passed"
	}
	view.Verification.MembersDigest, err = projectpack.MembersDigest(files, plan.Permissions)
	if err != nil {
		return fail(err)
	}
	plan.Files = files
	if err := publishPack(ctx, opts, packing, plan); err != nil {
		return fail(err)
	}
	view.Files, view.RequiresBuild = files, false
	return reportPack(view, jsonOutput, false)
}

// Resolve existing directory ancestors so a host alias such as macOS /var
// does not make an output inside the canonical project root appear external.
// The final filename is left intact for the writer's symlink checks.
func canonicalPackPath(output string) (string, error) {
	dir := filepath.Dir(output)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Join(resolved, filepath.Base(output)), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(dir) == dir {
			return "", err
		}
		missing = append(missing, filepath.Base(dir))
		dir = filepath.Dir(dir)
	}
}

func publishPack(ctx context.Context, opts compileOptions, packing packOptions, plan projectpack.Plan) error {
	prepared, err := projectpack.Prepare(ctx, packing.output, plan.Files, plan.Permissions)
	if err != nil {
		return err
	}
	defer func() { _ = prepared.Close() }()
	if packing.outputTemplate == "" {
		return prepared.Commit()
	}
	rel, err := filepath.Rel(opts.projectRoot, packing.output)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(rel) {
		return prepared.Commit()
	} // External ZIPs cannot become project inputs.
	group := "pack\x00" + opts.entry + "\x00" + opts.target + "\x00" + packing.mode
	return exporthistory.Publish(
		opts.projectRoot,
		group,
		exporthistory.File{
			Path:   filepath.ToSlash(rel),
			Size:   prepared.Size,
			SHA256: prepared.SHA256,
		},
		nil,
		prepared.Commit,
	)
}

func packBuildError(stage string, build client.CompileOutput) error {
	for _, log := range [][]byte{build.Stdout, build.Stderr} {
		if len(log) > 0 {
			terminalFprintf(os.Stderr, "%s", string(log))
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
		terminalPrintf("pack preview (%s): %s\n", view.Mode, view.Output)
	} else {
		terminalPrintf("packed %d files (%s): %s\n", len(view.Files), view.Mode, view.Output)
	}
	for _, file := range view.Files {
		terminalPrintf("%10d  %s  %s\n", file.Size, file.SHA256, file.Path)
	}
	if dryRun && view.Plan != nil {
		for _, member := range view.Plan.Members {
			if member.Action == "exclude" {
				terminalPrintf("excluded  %s  %s\n", member.Path, member.Reason)
			}
		}
		for _, resource := range view.Plan.Resources {
			terminalPrintf("resource  %s  %s  %s\n", resource.ID, resource.Action, resource.Reason)
		}
	}
	for _, warning := range view.Warnings {
		terminalFprintln(os.Stderr, "latexmk: warning:", terminalText(warning))
	}
	if dryRun && view.RequiresBuild {
		terminalPrintln("source preview only; remote build and generated submission files are pending")
	}
	if view.Verified {
		terminalPrintf("verified (%s): %s %s\n", view.Verification.Scope,
			view.Verification.EnvironmentID, view.Verification.EnvironmentDigest)
	}
	return 0
}
