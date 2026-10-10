package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func selectionClient(opts compileOptions) *client.Client {
	return &client.Client{ProjectRoot: opts.projectRoot, Exclude: opts.exclude, IgnoreFiles: opts.ignoreFiles,
		DenyFiles: opts.denyFiles, UnmatchedGlob: opts.unmatchedGlob, RespectGitIgnore: opts.gitIgnore,
		UploadMode: opts.uploadMode, ManifestFile: opts.manifestFile, IncludeFiles: opts.includeFiles}
}

func hasOption(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
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

func runAllTargets(args []string, cfg config.Resolved) int {
	if hasOption(args, "--realtime") || hasOption(args, "--watch") || hasOption(args, "--detach") {
		return fail(errors.New("--target all does not support watch or detach"))
	}
	if _, ok := cfg.Targets["all"]; ok {
		return fail(errors.New("target name \"all\" is reserved"))
	}
	var names []string
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fail(errors.New("no build targets configured"))
	}
	var common []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--target" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--target=") {
			continue
		}
		common = append(common, args[i])
	}
	code := 0
	for _, name := range names {
		next := append(append([]string{}, common...), "--target", name)
		if result := runCompile(next, "", hasOption(args, "--dry-run")); result != 0 {
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
	return client.ExportArtifact(opts.outDir, opts.projectRoot, opts.pdfExport, *chosen)
}

func optionsFromConfig(cfg config.Resolved, dryRun bool) compileOptions {
	return compileOptions{
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
		dryRun:        dryRun,
		watchInterval: cfg.Watch.Interval,
		watchDebounce: cfg.Watch.Debounce,
		watchMaxWait:  cfg.Watch.MaxWait,
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

func applyBuildTarget(opts *compileOptions, cfg config.Resolved, args []string) error {
	target, ok := cfg.Targets[opts.target]
	if !ok {
		return fmt.Errorf("unknown target %q", opts.target)
	}
	if opts.entry != "" {
		return errors.New("--target cannot be combined with an entry")
	}
	opts.entry, opts.pdfExport = target.Entry, target.PDF
	if cfg.ConfigPath != "" && !filepath.IsAbs(opts.entry) {
		opts.entry = filepath.Join(filepath.Dir(cfg.ConfigPath), opts.entry)
	}
	if target.Engine != "" && !opts.engineExplicit {
		opts.engine = target.Engine
	}
	if target.OutDir != "" && !hasOption(args, "--out-dir") && !hasOption(args, "-output-directory") {
		opts.outDir = target.OutDir
	}
	opts.includeFiles = append(opts.includeFiles, target.IncludeFiles...)
	return nil
}
