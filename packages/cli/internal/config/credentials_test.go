package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLazyAuthenticationAndPriority(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	t.Setenv("LATEXMK_TOKEN_FILE", filepath.Join(root, "missing"))
	t.Setenv("LATEXMK_TOKEN", "environment")
	cfg, err := Load(root, CredentialOptions{Token: "explicit", TokenSet: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "explicit" {
		t.Fatalf("token precedence: %v", err)
	}
	cfg, err = Load(root, CredentialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "environment" {
		t.Fatalf("environment precedence: %v", err)
	}
	t.Setenv("LATEXMK_TOKEN", "")
	cfg, err = Load(root, CredentialOptions{TokenFile: "missing", TokenFileSet: true})
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
	cfg, err := Load(t.TempDir(), CredentialOptions{})
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
	cfg, err := Load(root, CredentialOptions{})
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
	cfg, err = Load(root, CredentialOptions{EnvFile: new(string)})
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
	cfg, err := Load(t.TempDir(), CredentialOptions{})
	if err == nil {
		err = cfg.Authenticate("")
	}
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
		cfg, err := Load(root, CredentialOptions{})
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
	cfg, err := Load(project, CredentialOptions{})
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

func TestAuthenticationRejectsWhitespaceAndControlTokens(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	for _, token := range []string{"secret with-space", "secret\twith-tab", "secret\x00with-control"} {
		cfg, err := Load(root, CredentialOptions{Token: token, TokenSet: true})
		if err != nil {
			t.Fatal(err)
		}
		err = cfg.Authenticate(root)
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe authentication error: %v", err)
		}
		path := filepath.Join(root, "credential")
		if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadTokenFile(path); err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe token file error: %v", err)
		}
	}
}

func TestCredentialOptionsRejectExplicitEmptyAndConflictingValues(t *testing.T) {
	isolateUserConfig(t)
	for _, options := range []CredentialOptions{{TokenSet: true}, {TokenFileSet: true}, {TokenModeSet: true}, {Token: "secret", TokenFile: "secret-file"}, {TokenSet: true, TokenFileSet: true}} {
		if _, err := Load(t.TempDir(), options); err == nil {
			t.Errorf("accepted malformed credential options %+v", options)
		}
	}
}

func TestCredentialOptionsRemainLazyAndPreserveParsedValues(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	t.Setenv("LATEXMK_TOKEN", "environment")
	for _, token := range []string{"explicit", "--json", "a=b", `"quoted"`} {
		cfg, err := Load(root, CredentialOptions{Token: token})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Token != "" {
			t.Fatal("configuration loading performed authentication")
		}
		if err = cfg.Authenticate(root); err != nil || cfg.Token != token {
			t.Fatalf("parsed token changed or lost priority: %v", err)
		}
	}
	missing := filepath.Join(root, "missing-secret")
	cfg, err := Load(
		root,
		CredentialOptions{TokenFile: missing, TokenFileSet: true, TokenMode: "none", TokenModeSet: true},
	)
	if err != nil {
		t.Fatalf("loading read a credential file: %v", err)
	}
	if !slices.Contains(cfg.DenyFiles, missing) {
		t.Fatal("explicit credential path was not denied before authentication")
	}
	if err = cfg.Authenticate(root); err != nil || cfg.Token != "" {
		t.Fatalf("none mode attempted explicit credential file access: %v", err)
	}
	cfg, err = Load(root, CredentialOptions{TokenMode: "unsupported", TokenModeSet: true})
	if err != nil {
		t.Fatalf("token-mode validation moved before authentication: %v", err)
	}
	if err = cfg.Authenticate(root); err == nil {
		t.Fatal("authentication accepted an unsupported token mode")
	}
}

func TestConfigurationRejectsCaseVariantLiteralTokensAndNull(t *testing.T) {
	isolateUserConfig(t)
	for _, payload := range []string{`{"Token":"secret"}`, `{"TOKEN":"secret"}`, `{"token":{"env":"TOKEN"},"Token":"secret"}`, `null`} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, FileName), []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root, CredentialOptions{}); err == nil {
			t.Errorf("accepted configuration %s", payload)
		}
	}
}

func TestCaseVariantSourceKeysAndSecretSymlinks(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tokenFile, filepath.Join(root, "secret")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(root, FileName),
		[]byte(`{"Token":{"file":"secret"},"SERVER":"https://test.example"}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root, CredentialOptions{})
	if err == nil {
		err = cfg.Authenticate(root)
	}
	if err != nil || cfg.Token != "secret-token" || cfg.Server != "https://test.example" {
		t.Fatalf("case variant sources: cfg=%+v err=%v", cfg, err)
	}
}
