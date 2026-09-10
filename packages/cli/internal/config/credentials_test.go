package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLazyAuthenticationAndPriority(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	t.Setenv("LATEXMK_TOKEN_FILE", filepath.Join(root, "missing"))
	t.Setenv("LATEXMK_TOKEN", "environment")
	cfg, _, err := LoadArgs(root, []string{"--token", "explicit", "main.tex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "explicit" {
		t.Fatalf("token precedence: %v", err)
	}
	cfg, _, err = LoadArgs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "environment" {
		t.Fatalf("environment precedence: %v", err)
	}
	t.Setenv("LATEXMK_TOKEN", "")
	cfg, _, err = LoadArgs(root, []string{"--token-file", "missing"})
	if err != nil {
		t.Fatalf("preview tried to read a token file: %v", err)
	}
	if err := cfg.Authenticate(root); err == nil {
		t.Fatal("explicit missing file accepted")
	}
}

func TestDefaultTokenAtResolvedRoot(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, TokenFileName), []byte("local-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadArgs(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "local-token" {
		t.Fatalf("default token: %v", err)
	}
	cfg.TokenMode = "none"
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "" {
		t.Fatal("none must disable authentication")
	}
}

func TestDotenvDoesNotMutateProcessOrExecuteShell(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("LATEXMK_SERVER", "")
	root := t.TempDir()
	content := "LATEXMK_ENGINE=pdflatex\nLATEXMK_TOKEN_FILE=secret\nUNRELATED=$(touch should-not-exist)\n"
	if err := os.WriteFile(filepath.Join(root, EnvFileName), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("dotenv-token"), 0600); err != nil {
		t.Fatal(err)
	}
	// A process value, including empty, has precedence. Remove only inside this test.
	previous, present := os.LookupEnv("LATEXMK_TOKEN_FILE")
	if err := os.Unsetenv("LATEXMK_TOKEN_FILE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("LATEXMK_TOKEN_FILE", previous)
		} else {
			_ = os.Unsetenv("LATEXMK_TOKEN_FILE")
		}
	})
	cfg, _, err := LoadArgs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "pdflatex" {
		t.Fatalf("dotenv engine: %s", cfg.Engine)
	}
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "dotenv-token" {
		t.Fatalf("relative dotenv token: %v", err)
	}
	if os.Getenv("UNRELATED") != "" {
		t.Fatal("dotenv mutated process")
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("shell executed")
	}
	cfg, _, err = LoadArgs(root, []string{"--no-env-file"})
	if err != nil || cfg.EnvPath != "" {
		t.Fatalf("no env file: %v", err)
	}
}

func TestConfiguredTokenFileRelativeToItsConfig(t *testing.T) {
	userDir := filepath.Join(isolateUserConfig(t), "latexmk")
	if err := os.MkdirAll(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(userDir, UserFileName),
		[]byte(`{"tokenFile":"token.txt"}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "token.txt"), []byte("user-file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(t.TempDir())
	if err != nil || cfg.Token != "user-file" || !strings.Contains(cfg.TokenSource, "user config") {
		t.Fatalf("user credential source: %v", err)
	}
}

func TestLegacyAuxiliaryConfigurationPreserved(t *testing.T) {
	isolateUserConfig(t)
	for _, test := range []struct{ json, local, server string }{
		{`{"server":"http://localhost:8080"}`, "output", "retain"},
		{`{"auxiliary":{"server":"none"}}`, "output", "none"},
		{`{"auxiliary":{"server":"reuse"}}`, "output", "reuse"},
		{`{"auxiliary":{"local":"none","server":"none"}}`, "none", "none"},
	} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, FileName), []byte(test.json), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := LoadArgs(root, nil)
		if err != nil || cfg.Auxiliary.Local != test.local || cfg.Auxiliary.Server != test.server {
			t.Fatalf("legacy defaults: %+v %v", cfg.Auxiliary, err)
		}
	}
}

func TestUserPolicyAndDefaultCredentialsAreDeniedBeforeAuthentication(t *testing.T) {
	base := isolateUserConfig(t)
	userDir := filepath.Join(base, "latexmk")
	if err := os.MkdirAll(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(userDir, UserFileName)
	if err := os.WriteFile(userPath, []byte(`{"projectRoot":"papers"}`), 0600); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, FileName), []byte(`{"engine":"pdflatex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := LoadArgs(project, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoot != filepath.Join(userDir, "papers") {
		t.Fatalf("user root rebased to project: %s", cfg.ProjectRoot)
	}
	for _, name := range []string{userPath, filepath.Join(userDir, "token")} {
		found := false
		for _, denied := range cfg.DenyFiles {
			if denied == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("credential path absent from preview policy: %s", name)
		}
	}
}
