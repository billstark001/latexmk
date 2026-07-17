package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
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

func TestParseCompileArgsUploadMode(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, uploadMode: "auto"}
	if err := parseCompileArgs([]string{"--upload-mode", "all", "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.uploadMode != "all" {
		t.Fatalf("upload mode = %q, want all", opts.uploadMode)
	}
	if err := parseCompileArgs([]string{"--upload-mode", "legacy", "main.tex"}, &opts); err == nil {
		t.Fatal("expected unsupported upload mode to fail")
	}
}

func TestParseCompileArgsManifestFiles(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, uploadMode: "auto"}
	args := []string{"--upload-mode", "manifest", "--manifest", ".latexmk-files", "--include-file", "chapter.tex", "--include-file=figure.pdf", "main.tex"}
	if err := parseCompileArgs(args, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.uploadMode != "manifest" || opts.manifestFile != ".latexmk-files" {
		t.Fatalf("manifest options = %#v", opts)
	}
	if len(opts.includeFiles) != 2 || opts.includeFiles[0] != "chapter.tex" || opts.includeFiles[1] != "figure.pdf" {
		t.Fatalf("include files = %#v", opts.includeFiles)
	}
}

func TestParseCompileArgsWatchOptions(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, watchInterval: 500 * time.Millisecond, watchDebounce: 500 * time.Millisecond}
	if err := parseCompileArgs([]string{"--watch", "--watch-interval", "25ms", "--watch-debounce=75ms", "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if !opts.watch || opts.watchInterval != 25*time.Millisecond || opts.watchDebounce != 75*time.Millisecond {
		t.Fatalf("watch options = %#v", opts)
	}
	opts.watchInterval = 0
	if err := parseCompileArgs([]string{"--watch", "main.tex"}, &opts); err == nil {
		t.Fatal("expected zero watch interval to fail")
	}
}

func TestSelectedFilesChangedIgnoresNewRecorderDependencies(t *testing.T) {
	before := []projectarchive.File{{Path: "main.tex", SHA256: "main-v1"}}
	after := []projectarchive.File{{Path: "main.tex", SHA256: "main-v1"}, {Path: "dynamic.tex", SHA256: "dynamic-v1"}}
	if selectedFilesChanged(before, after) {
		t.Fatal("a newly discovered dependency should not imply an edit during compilation")
	}
	after[0].SHA256 = "main-v2"
	if !selectedFilesChanged(before, after) {
		t.Fatal("an existing selected file change was not detected")
	}
}

func TestWatchTargetsOnlyAddsSelectedFilesAndPolicyControls(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(repo, "paper")
	if err := os.MkdirAll(filepath.Join(project, "sections"), 0o700); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(project, "main.tex")
	bodyPath := filepath.Join(project, "sections", "body.tex")
	targets := watchTargets(compileOptions{projectRoot: project, gitIgnore: true, manifestFile: ".latexmk-files"}, []projectarchive.File{
		{Path: "main.tex", Source: mainPath},
		{Path: "sections/body.tex", Source: bodyPath},
	})
	paths := make(map[string]bool)
	for _, target := range targets {
		paths[target.Path] = true
	}
	for _, required := range []string{
		mainPath,
		bodyPath,
		filepath.Join(project, ".latexmk-files"),
		filepath.Join(project, ".gitignore"),
		filepath.Join(project, "sections", ".gitignore"),
		filepath.Join(repo, ".gitignore"),
		filepath.Join(repo, ".git", "info", "exclude"),
	} {
		if !paths[required] {
			t.Errorf("missing watch target %s", required)
		}
	}
	if paths[filepath.Join(project, "unrelated-secret.txt")] {
		t.Fatal("watcher included an unrelated project file")
	}
}
