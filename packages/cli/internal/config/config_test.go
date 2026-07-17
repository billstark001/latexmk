package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsToEntryRootModeWithoutExpandingToGitRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "papers", "demo")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LATEXMK_ROOT_MODE", "")
	cfg, err := Load(nested)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootMode != "entry" {
		t.Fatalf("root mode = %q, want entry", cfg.RootMode)
	}
	if cfg.ProjectRoot != "" {
		t.Fatalf("project root = %q, want empty", cfg.ProjectRoot)
	}
}

func TestFindGitRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := FindGitRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if root != repo {
		t.Fatalf("root = %q, want %q", root, repo)
	}
}

func TestDefaultDenyIncludesSensitiveLocalConfiguration(t *testing.T) {
	want := map[string]bool{".latexmk.json": false, ".env": false, "*.key": false, "*.pem": false}
	for _, pattern := range DefaultDeny() {
		if _, ok := want[pattern]; ok {
			want[pattern] = true
		}
	}
	for pattern, found := range want {
		if !found {
			t.Errorf("default excludes missing %q", pattern)
		}
	}
}

func TestLoadKeepsDefaultDenyWhenProjectReplacesExcludes(t *testing.T) {
	root := t.TempDir()
	configJSON := `{"server":"http://127.0.0.1:8080","exclude":["custom.tmp"]}`
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"custom.tmp": false, FileName: false, ".env": false, "*.key": false}
	for _, pattern := range cfg.Exclude {
		if _, ok := want[pattern]; ok {
			want[pattern] = true
		}
	}
	for pattern, found := range want {
		if !found {
			t.Errorf("resolved excludes missing %q", pattern)
		}
	}
}
