package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/argv"
)

type cacheOptions struct {
	root       string
	jsonOutput bool
	local      bool
}

func runCache(args []string) int {
	if len(args) > 0 && args[0] == "resources" {
		return runResourceCache(args[1:])
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent("cache", false, err)
	}
	changes := new(argv.Options[cacheOptions])
	changes.Value("project-root", func(options *cacheOptions, value string) error { options.root = value; return nil })
	changes.Bool("json", func(options *cacheOptions, value bool) error { options.jsonOutput = value; return nil })
	changes.Bool("local", func(options *cacheOptions, value bool) error { options.local = value; return nil })
	cfg, parsed, err := loadCommand("cache", cwd, args, changes.Flags()...)
	jsonOutput := parsed != nil && parsed.Bool("json")
	if err != nil {
		return failAgentArguments("cache", jsonOutput, err)
	}
	positionals := parsed.Args().Slice()
	if len(positionals) != 1 || (positionals[0] != "ignore" && positionals[0] != "clean") {
		return failAgentArguments("cache", jsonOutput, errors.New("cache supports 'ignore' and 'clean'"))
	}
	opts := cacheOptions{root: cfg.ProjectRoot}
	if opts.root == "" {
		opts.root = cwd
	}
	if err := changes.Apply(&opts); err != nil {
		return failAgentArguments("cache."+positionals[0], jsonOutput, err)
	}
	root := opts.root
	if positionals[0] == "clean" {
		if opts.local {
			return failAgentArguments("cache.clean", jsonOutput, errors.New("--local requires cache ignore"))
		}
		return cleanLocalCache(root, opts.jsonOutput)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return failAgent("cache.ignore", jsonOutput, err)
	}
	result, err := client.AddProjectCacheGitIgnore(root, opts.local, cfg.ResourceOptions.LockFile)
	if err != nil {
		return failAgent("cache.ignore", jsonOutput, err)
	}
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return fail(err)
		}
		return 0
	}
	if result.Changed {
		terminalPrintf("added .latexmk-cache/ to %s\n", result.GitIgnore)
	} else {
		terminalPrintln(".latexmk-cache is already covered by the effective Git ignore rules")
	}
	terminalPrintln("warning: git clean -fdX deletes ignored cache files and resets the local project identity")
	return 0
}

func cleanLocalCache(root string, jsonOutput bool) int {
	fs, err := os.OpenRoot(root)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = fs.Close() }()
	info, err := fs.Lstat(".latexmk-cache")
	removed := false
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail(errors.New(".latexmk-cache must be a real directory"))
		}
		if err := fs.RemoveAll(client.AuxiliaryCachePath()); err != nil {
			return fail(err)
		}
		removed = true
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	if jsonOutput {
		if err := writeAgentJSON("cache.clean", map[string]bool{"cleaned": removed}); err != nil {
			return fail(err)
		}
	} else {
		terminalPrintln("local auxiliary cache cleared; project identity and dependency history preserved")
	}
	return 0
}
