package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
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

func runAllTargets(args []string, cfg config.Resolved) int {
	if hasOption(args, "--watch") || hasOption(args, "--detach") {
		return fail(errors.New("--target all does not support watch or detach"))
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
