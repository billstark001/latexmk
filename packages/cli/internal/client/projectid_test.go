package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveProjectIDPersistsRandomIdentity(t *testing.T) {
	root := t.TempDir()
	first, err := ResolveProjectIDWithStatus(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !validProjectID(first.ID) {
		t.Fatalf("unexpected first resolution: %#v", first)
	}
	second, err := ResolveProjectIDWithStatus(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.ID != first.ID {
		t.Fatalf("identity was not stable: first=%#v second=%#v", first, second)
	}
	info, err := os.Stat(filepath.Join(root, projectIDRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("project ID permissions = %o, want private", info.Mode().Perm())
	}
}

func TestResolveProjectIDRejectsSymlinkedCache(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "project-id"), []byte("project-attacker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, ".latexmk-cache")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProjectID(root, true); err == nil {
		t.Fatal("expected symlinked cache directory to be rejected")
	}
}

func TestCacheIgnoreCoversNegatedEntry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.Mkdir(filepath.Join(root, ".latexmk-cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".latexmk-cache", "nested"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".latexmk-cache/*\n!.latexmk-cache/nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := InspectProjectCacheGitIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ignored {
		t.Fatal("negated cache entry must not be reported as fully ignored")
	}
	result, err := AddProjectCacheGitIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("expected an explicit trailing cache rule")
	}
}
