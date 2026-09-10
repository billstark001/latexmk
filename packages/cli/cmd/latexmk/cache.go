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
	jsonOutput := hasJSONFlag(args)
	if len(args) == 0 || args[0] != "ignore" {
		return failAgentArguments("cache", jsonOutput, errors.New("cache currently supports only 'ignore'"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent("cache.ignore", jsonOutput, err)
	}
	cfg, err := config.Load(cwd)
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
