package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	sharedresources "github.com/billstark001/latexmk/packages/shared/resources"
)

func selectionClient(opts compileOptions) *client.Client {
	c := &client.Client{
		ProjectRoot: opts.projectRoot, Exclude: opts.exclude, IgnoreFiles: opts.ignoreFiles,
		GeneratedFiles: generatedFiles(opts),
		DenyFiles:      opts.denyFiles, UnmatchedGlob: opts.unmatchedGlob, RespectGitIgnore: opts.gitIgnore,
		UploadMode: opts.uploadMode, ManifestFile: opts.manifestFile, IncludeFiles: opts.includeFiles}
	if opts.resourceCapture != nil {
		c.ResourceFiles = opts.resourceCapture.MountFiles()
		c.DenyFiles = append(c.DenyFiles, opts.resourceCapture.DenyPaths...)
	} else {
		paths, _ := cliresources.PrivatePaths(opts.projectRoot, opts.resourceSpecs)
		c.DenyFiles = append(c.DenyFiles, paths...)
	}
	lockPath := filepath.Join(opts.projectRoot, cliresources.LockName(opts.resourceOptions.LockFile))
	c.DenyFiles = append(c.DenyFiles, lockPath, lockPath+".guard")
	if opts.resourceCapture != nil {
		if lock, err := cliresources.LoadLock(opts.projectRoot, opts.resourceOptions.LockFile); err == nil {
			req, err := sharedresources.NormalizeRequest(
				sharedresources.ResolveRequest{
					Baseline:  opts.environment,
					Resources: opts.resourceCapture.Resources,
					Requires:  opts.requires,
				},
			)
			if err == nil {
				digest, _ := sharedresources.Hash(req)
				if plan, ok := lock.Plans[resourcePlanKey(opts)]; ok && plan.SpecDigest == digest {
					c.SetBaseline(plan.Environment.Baseline)
					c.CacheEnvironment = plan.Environment.Digest
				}
			}
		}
	}
	return c
}

func implicitTarget(cfg config.Resolved) (string, error) {
	if cfg.DefaultTarget != "" {
		if cfg.DefaultTarget == "all" {
			return "", errors.New("defaultTarget cannot be the reserved target name \"all\"")
		}
		if _, ok := cfg.Targets[cfg.DefaultTarget]; !ok {
			return "", fmt.Errorf("defaultTarget %q is not a configured target", cfg.DefaultTarget)
		}
		return cfg.DefaultTarget, nil
	}
	if len(cfg.Targets) == 1 {
		for name := range cfg.Targets {
			if name == "all" {
				return "", errors.New("target name \"all\" is reserved")
			}
			return name, nil
		}
	}
	if len(cfg.Targets) > 1 {
		names := make([]string, 0, len(cfg.Targets))
		for name := range cfg.Targets {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", fmt.Errorf(
			"multiple build targets configured (%s); use --target NAME or set defaultTarget",
			strings.Join(names, ", "),
		)
	}
	return "", nil
}

func runAllTargets(cwd string, cfg config.Resolved, opts compileOptions, parsed *cli.Command) int {
	if opts.realtime || opts.watch || opts.detach {
		return fail(errors.New("--target all does not support watch or detach"))
	}
	if opts.entry != "" {
		return fail(errors.New("--target cannot be combined with an entry"))
	}
	if _, ok := cfg.Targets["all"]; ok {
		return fail(errors.New("target name \"all\" is reserved"))
	}
	names := make([]string, 0, len(cfg.Targets))
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fail(errors.New("no build targets configured"))
	}
	code := 0
	for _, name := range names {
		target := opts
		target.target = name
		// Each target owns the mutable option lists it extends during execution.
		target.includeFiles = slices.Clone(opts.includeFiles)
		target.ignoreFiles = slices.Clone(opts.ignoreFiles)
		target.denyFiles = slices.Clone(opts.denyFiles)
		target.exclude = slices.Clone(opts.exclude)
		target.resourceIDs = slices.Clone(opts.resourceIDs)
		target.resourceAddIDs = slices.Clone(opts.resourceAddIDs)
		target.resourceRemoveIDs = slices.Clone(opts.resourceRemoveIDs)
		target.controlFiles = slices.Clone(opts.controlFiles)
		if err := applyBuildTarget(&target, cfg, parsed); err != nil {
			_ = fail(err)
			code = 1
			continue
		}
		if result := runResolvedCompile(cwd, cfg, target); result != 0 {
			code = result
		}
	}
	return code
}

func exportPDF(opts compileOptions, out client.CompileOutput) error {
	if opts.pdfExport == "" || !out.Result.Success {
		return nil
	}
	stem := opts.jobName
	if stem == "" {
		stem = strings.TrimSuffix(filepath.Base(opts.entry), filepath.Ext(opts.entry))
	}
	var chosen *protocol.Artifact
	for i := range out.Result.Artifacts {
		file := &out.Result.Artifacts[i]
		if filepath.Base(file.Path) == stem+".pdf" {
			if chosen != nil {
				return fmt.Errorf("multiple PDFs match target %s", stem)
			}
			chosen = file
		}
	}
	if chosen == nil {
		return fmt.Errorf("target PDF %s.pdf was not returned", stem)
	}
	return publishPDF(opts, *chosen)
}

func optionsFromConfig(cfg config.Resolved, dryRun bool) compileOptions {
	return compileOptions{
		resourceSpecs:   cfg.Resources,
		resourceOptions: cfg.ResourceOptions,
		resourceIDs:     cfg.DefaultResources,
		publications:    cfg.Publications,
		pdfTemplate:     cfg.PDFTemplate,
		pdfKeepLast:     retentionCount(cfg.PDFRetention),
		nameScope:       cfg.Naming.Scope,
		nameTimeZone:    cfg.Naming.TimeZone,
		ignoreFiles:     cfg.IgnoreFiles,
		denyFiles:       cfg.DenyFiles,
		unmatchedGlob:   cfg.UnmatchedGlob,
		auxiliary:       cfg.Auxiliary,
		server:          cfg.Server,
		token:           cfg.Token,
		projectRoot:     cfg.ProjectRoot,
		projectID:       cfg.ProjectID,
		rootMode:        cfg.RootMode,
		uploadMode:      cfg.UploadMode,
		manifestFile:    cfg.ManifestFile,
		includeFiles:    append([]string(nil), cfg.IncludeFiles...),
		gitIgnore:       cfg.RespectGitIgnore,
		engine:          cfg.Engine,
		outDir:          cfg.OutDir,
		timeout:         cfg.Timeout,
		interaction:     "nonstopmode",
		synctex:         true,
		haltOnError:     true,
		fileLineError:   true,
		insecure:        cfg.InsecureSkipVerify,
		exclude:         cfg.Exclude,
		configPath:      cfg.ConfigPath,
		dryRun:          dryRun,
		watchInterval:   cfg.Watch.Interval,
		watchDebounce:   cfg.Watch.Debounce,
		watchMaxWait:    cfg.Watch.MaxWait,
	}
}

func requestFromOptions(opts compileOptions) protocol.CompileRequest {
	return protocol.CompileRequest{
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
}

func applyBuildTarget(opts *compileOptions, cfg config.Resolved, parsed *cli.Command) error {
	target, ok := cfg.Targets[opts.target]
	if !ok {
		return fmt.Errorf("unknown target %q", opts.target)
	}
	if opts.entry != "" {
		return errors.New("--target cannot be combined with an entry")
	}
	opts.entry = target.Entry
	if !parsed.IsSet("pdf-template") {
		if target.PDF != "" || target.PDFTemplate != "" {
			opts.pdfExport, opts.pdfTemplate = target.PDF, target.PDFTemplate
		}
	}
	if target.PDFRetention != nil && !parsed.IsSet("pdf-keep-last") {
		opts.pdfKeepLast = target.PDFRetention.KeepLast
	}
	if cfg.ConfigPath != "" && !filepath.IsAbs(opts.entry) {
		opts.entry = filepath.Join(filepath.Dir(cfg.ConfigPath), opts.entry)
	}
	if target.Engine != "" && !opts.engineExplicit {
		opts.engine = target.Engine
	}
	if target.OutDir != "" && !parsed.IsSet("out-dir") {
		opts.outDir = target.OutDir
	}
	if target.Resources != nil && !opts.resourceExplicit {
		opts.resourceIDs = append([]string{}, target.Resources...)
	}
	opts.requires = target.Requires
	if target.Environment != "" && !opts.environmentExplicit {
		opts.environment = target.Environment
	}
	opts.includeFiles = append(opts.includeFiles, target.IncludeFiles...)
	return nil
}
