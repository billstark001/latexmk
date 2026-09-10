package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidLimitFailsFast(t *testing.T) {
	t.Setenv("LATEXMK_MAX_FILES", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid environment variable error")
	}
}

func TestTokenMustBeLong(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "token")
	t.Setenv("LATEXMK_API_TOKEN", "short")
	if _, err := Load(); err == nil {
		t.Fatal("expected short token error")
	}
}

func TestAPITokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	token := "a-secure-token-value-at-least-24-characters"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LATEXMK_AUTH_MODE", "token")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken != token {
		t.Fatalf("token = %q", cfg.APIToken)
	}
}

func TestAPITokenAndFileAreMutuallyExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("another-secure-token-at-least-24-characters\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LATEXMK_API_TOKEN", "a-secure-token-value-at-least-24-characters")
	t.Setenv("LATEXMK_API_TOKEN_FILE", path)
	if _, err := Load(); err == nil {
		t.Fatal("expected mutually exclusive token sources to fail")
	}
}

func TestLegacyCompileDisabledByDefault(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnableLegacyCompile {
		t.Fatal("legacy compile should be disabled by default")
	}
}

func TestValidOriginRejectsWildcardsAndPaths(t *testing.T) {
	for _, origin := range []string{"*", "https://console.example.edu/path", "ftp://console.example.edu", "https://user:pass@console.example.edu"} {
		if validOrigin(origin) {
			t.Fatalf("expected invalid origin %q", origin)
		}
	}
	if !validOrigin("https://console.example.edu:8443") {
		t.Fatal("expected exact HTTPS origin to be valid")
	}
}
