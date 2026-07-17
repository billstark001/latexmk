package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeCompilePathsResolvesProjectRootSymlink(t *testing.T) {
	physicalRoot := t.TempDir()
	entry := filepath.Join(physicalRoot, "main.tex")
	if err := os.WriteFile(entry, []byte("\\documentclass{article}"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(physicalRoot, alias); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(physicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{projectRoot: alias, entry: "main.tex"}
	if err := normalizeCompilePaths(&opts, physicalRoot); err != nil {
		t.Fatal(err)
	}
	if opts.projectRoot != resolvedRoot {
		t.Fatalf("project root = %q, want %q", opts.projectRoot, resolvedRoot)
	}
	if opts.entry != "main.tex" {
		t.Fatalf("entry = %q, want main.tex", opts.entry)
	}
}

func TestNormalizeCompilePathsDefaultsToEntryDirectory(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "paper")
	entry := filepath.Join(project, "main.tex")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("\\documentclass{article}"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{rootMode: "entry", entry: filepath.Join("paper", "main.tex")}
	if err := normalizeCompilePaths(&opts, parent); err != nil {
		t.Fatal(err)
	}
	resolvedProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if opts.projectRoot != resolvedProject {
		t.Fatalf("project root = %q, want %q", opts.projectRoot, resolvedProject)
	}
	if opts.entry != "main.tex" {
		t.Fatalf("entry = %q, want main.tex", opts.entry)
	}
}

func TestNormalizeCompilePathsUsesGitRootOnlyWhenRequested(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(repo, "papers", "demo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(project, "main.tex")
	if err := os.WriteFile(entry, []byte("\\documentclass{article}"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{rootMode: "git", entry: "main.tex"}
	if err := normalizeCompilePaths(&opts, project); err != nil {
		t.Fatal(err)
	}
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if opts.projectRoot != resolvedRepo {
		t.Fatalf("project root = %q, want %q", opts.projectRoot, resolvedRepo)
	}
	if opts.entry != "papers/demo/main.tex" {
		t.Fatalf("entry = %q, want papers/demo/main.tex", opts.entry)
	}
}

func TestParseCompileArgsRejectsUnknownRootMode(t *testing.T) {
	opts := compileOptions{timeout: time.Minute}
	if err := parseCompileArgs([]string{"--root-mode", "parent", "main.tex"}, &opts); err == nil {
		t.Fatal("expected invalid root mode error")
	}
}

func TestParseCompileArgsReadsTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{timeout: time.Minute}
	if err := parseCompileArgs([]string{"--token-file", path, "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.token != "file-token" {
		t.Fatalf("token = %q, want file-token", opts.token)
	}
}
