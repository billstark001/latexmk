package config

import (
	"os"
	"path/filepath"
	"testing"
)

func isolateUserConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LATEXMK_TOKEN", "")
	t.Setenv("LATEXMK_TOKEN_FILE", "")
	t.Setenv("LATEXMK_UPLOAD_MODE", "")
	t.Setenv("LATEXMK_MANIFEST_FILE", "")
	return dir
}

func TestLoadDefaultsToAutomaticDependencySelection(t *testing.T) {
	isolateUserConfig(t)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadMode != "auto" {
		t.Fatalf("upload mode = %q, want auto", cfg.UploadMode)
	}
}

func TestLoadRejectsUnknownUploadMode(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(`{"uploadMode":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("expected invalid uploadMode to fail")
	}
}

func TestLoadManifestConfiguration(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	configJSON := `{"uploadMode":"manifest","manifestFile":".latexmk-files","includeFiles":["chapter.tex"]}`
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadMode != "manifest" || cfg.ManifestFile != ".latexmk-files" || len(cfg.IncludeFiles) != 1 || cfg.IncludeFiles[0] != "chapter.tex" {
		t.Fatalf("manifest config = %#v", cfg)
	}
}

func TestLoadDefaultsToEntryRootModeWithoutExpandingToGitRoot(t *testing.T) {
	isolateUserConfig(t)
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
	want := map[string]bool{".latexmk.json": false, ".latexmk-files": false, ".env": false, "*.key": false, "*.pem": false}
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
	isolateUserConfig(t)
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

func TestLoadRespectsExplicitGitIgnoreSetting(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	configJSON := `{"server":"http://127.0.0.1:8080","respectGitignore":false}`
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RespectGitIgnore {
		t.Fatal("respectGitignore = true, want false")
	}
}

func TestLoadTokenPrecedence(t *testing.T) {
	configHome := isolateUserConfig(t)
	root := t.TempDir()
	userDir := filepath.Join(configHome, "latexmk")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, UserFileName), []byte(`{"token":"user-token","server":"https://user.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(`{"token":"project-token","server":"https://project.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "user-token" {
		t.Fatalf("token = %q, want user-token", cfg.Token)
	}
	if cfg.Server != "https://project.example" {
		t.Fatalf("server = %q, want project config to override user config", cfg.Server)
	}

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LATEXMK_TOKEN_FILE", tokenFile)
	cfg, err = Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "file-token" {
		t.Fatalf("token = %q, want file-token", cfg.Token)
	}

	t.Setenv("LATEXMK_TOKEN", "environment-token")
	cfg, err = Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "environment-token" {
		t.Fatalf("token = %q, want environment-token", cfg.Token)
	}
}

func TestReadTokenFileRejectsMultipleLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenFile(path); err == nil {
		t.Fatal("expected multiple token lines to be rejected")
	}
}
