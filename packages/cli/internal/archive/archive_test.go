package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCreateExcludesDirectories(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "main.tex"), "test")
	mustWrite(t, filepath.Join(root, ".git", "config"), "secret")
	var buf bytes.Buffer
	stats, err := Create(&buf, Options{Root: root, Exclude: []string{".git"}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 {
		t.Fatalf("files=%d", stats.Files)
	}
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "main.tex" {
		t.Fatalf("name=%q", h.Name)
	}
	if _, err := io.Copy(io.Discard, tr); err != nil {
		t.Fatal(err)
	}
}

func TestManifestRespectsGitIgnoreAndKeepsTrackedFiles(t *testing.T) {
	root := t.TempDir()
	mustRun(t, root, "git", "init", "--quiet")
	mustWrite(t, filepath.Join(root, ".gitignore"), "ignored.txt\ntracked-ignored.txt\n")
	mustWrite(t, filepath.Join(root, "main.tex"), "main")
	mustWrite(t, filepath.Join(root, "untracked.tex"), "untracked")
	mustWrite(t, filepath.Join(root, "ignored.txt"), "secret")
	mustWrite(t, filepath.Join(root, "tracked-ignored.txt"), "tracked")
	mustRun(t, root, "git", "add", ".gitignore", "main.tex")
	mustRun(t, root, "git", "add", "--force", "tracked-ignored.txt")

	files, _, err := Manifest(Options{
		Root: root, Exclude: []string{".git", ".gitignore"}, RespectGitIgnore: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, file := range files {
		got[file.Path] = true
	}
	for _, included := range []string{"main.tex", "untracked.tex", "tracked-ignored.txt"} {
		if !got[included] {
			t.Errorf("manifest missing %q: %#v", included, got)
		}
	}
	if got["ignored.txt"] {
		t.Fatalf("manifest included Git-ignored file: %#v", got)
	}
}

func TestManifestCanDisableGitIgnore(t *testing.T) {
	root := t.TempDir()
	mustRun(t, root, "git", "init", "--quiet")
	mustWrite(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	mustWrite(t, filepath.Join(root, "main.tex"), "main")
	mustWrite(t, filepath.Join(root, "ignored.txt"), "secret")
	files, _, err := Manifest(Options{
		Root: root, Exclude: []string{".git", ".gitignore"}, RespectGitIgnore: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, file := range files {
		got[file.Path] = true
	}
	if !got["ignored.txt"] {
		t.Fatalf("manifest should include ignored file when Git filtering is disabled: %#v", got)
	}
}

func TestManifestRespectsGitIgnoreFromNestedProjectRoot(t *testing.T) {
	repo := t.TempDir()
	mustRun(t, repo, "git", "init", "--quiet")
	project := filepath.Join(repo, "papers", "demo")
	mustWrite(t, filepath.Join(repo, ".gitignore"), "papers/demo/private-note.txt\n")
	mustWrite(t, filepath.Join(project, "main.tex"), "main")
	mustWrite(t, filepath.Join(project, "figure.pdf"), "figure")
	mustWrite(t, filepath.Join(project, "private-note.txt"), "secret")
	mustRun(t, repo, "git", "add", ".gitignore", "papers/demo/main.tex")

	files, _, err := Manifest(Options{Root: project, RespectGitIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, file := range files {
		got[file.Path] = true
	}
	if !got["main.tex"] || !got["figure.pdf"] {
		t.Fatalf("nested manifest missed project files: %#v", got)
	}
	if got["private-note.txt"] {
		t.Fatalf("nested manifest included ignored file: %#v", got)
	}
}

func TestManifestAllowsNonGitDirectoryWhenGitIgnoreEnabled(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "main.tex"), "main")
	files, _, err := Manifest(Options{Root: root, RespectGitIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatalf("non-Git manifest = %#v", files)
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
