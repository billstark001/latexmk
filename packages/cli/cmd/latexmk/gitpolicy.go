package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxGitPolicyPathBytes = 16 << 10

// effectiveGitExcludesFile returns the global excludes file Git applies to
// this work tree. Watching a missing path also detects its later creation.
func effectiveGitExcludesFile(repoRoot string) (string, bool) {
	cmd := exec.Command("git", "-C", repoRoot, "config", "--path", "--get", "core.excludesFile")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return defaultGitExcludesFile(repoRoot)
		}
		return "", false
	}
	if len(output) == 0 || len(output) > maxGitPolicyPathBytes {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(output), "\n"), "\r")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	return resolveGitPolicyPath(repoRoot, value)
}

func defaultGitExcludesFile(repoRoot string) (string, bool) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		base = filepath.Join(home, ".config")
	}
	return resolveGitPolicyPath(repoRoot, filepath.Join(base, "git", "ignore"))
}

func resolveGitPolicyPath(repoRoot, value string) (string, bool) {
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		value = filepath.Join(home, strings.TrimLeft(value[1:], `/\`))
	} else if strings.HasPrefix(value, "~") {
		return "", false
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(repoRoot, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", false
	}
	return filepath.Clean(abs), true
}
