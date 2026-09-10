package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
)

func runCache(args []string) int {
	if len(args) > 0 && args[0] == "clean" {
		return runCacheClean(args[1:])
	}
	jsonOutput := hasJSONFlag(args)
	if len(args) == 0 || args[0] != "ignore" {
		return failAgentArguments("cache", jsonOutput, errors.New("cache supports 'ignore' and 'clean'"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent("cache.ignore", jsonOutput, err)
	}
	cfg, args, err := config.LoadArgs(cwd, args)
	if err != nil {
		return failAgent("cache.ignore", jsonOutput, err)
	}
	root := cfg.ProjectRoot
	if root == "" {
		root = cwd
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--project-root" && i+1 < len(args):
			i++
			root = args[i]
		case strings.HasPrefix(a, "--project-root="):
			root = strings.SplitN(a, "=", 2)[1]
		case a == "--json":
		default:
			return failAgentArguments("cache.ignore", jsonOutput, fmt.Errorf("unknown option %q", a))
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return failAgent("cache.ignore", jsonOutput, err)
	}
	result, err := client.AddProjectCacheGitIgnore(root)
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
		fmt.Printf("added .latexmk-cache/ to %s\n", result.GitIgnore)
	} else {
		fmt.Println(".latexmk-cache is already covered by the effective Git ignore rules")
	}
	fmt.Println("warning: git clean -fdX deletes ignored cache files and resets the local project identity")
	return 0
}

func runCacheClean(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, args, err := config.LoadArgs(cwd, args)
	if err != nil {
		return fail(err)
	}
	root := cfg.ProjectRoot
	if root == "" {
		root = cwd
	}
	jsonOutput := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--project-root" && i+1 < len(args):
			i++
			root = args[i]
		case strings.HasPrefix(args[i], "--project-root="):
			root = strings.TrimPrefix(args[i], "--project-root=")
		case args[i] == "--json":
			jsonOutput = true
		default:
			return fail(fmt.Errorf("unknown cache clean option %q", args[i]))
		}
	}
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
		if err := fs.RemoveAll(filepath.Join(".latexmk-cache", "aux")); err != nil {
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
		fmt.Println("local auxiliary cache cleared; project identity and dependency history preserved")
	}
	return 0
}
