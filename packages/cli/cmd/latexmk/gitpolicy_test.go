package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEffectiveGitExcludesFileUsesRepositoryConfiguration(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	if out, err := exec.Command(git, "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	want := filepath.Join(root, "policy", "global-ignore")
	if out, err := exec.Command(git, "-C", root, "config", "core.excludesFile", want).CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	got, ok := effectiveGitExcludesFile(root)
	if !ok || got != want {
		t.Fatalf("effective excludes = %q, %t; want %q", got, ok, want)
	}
}
