package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResourceCacheUserConfigAndEnvironmentPrecedence(t *testing.T) {
	user := isolateUserConfig(t)
	if err := os.MkdirAll(filepath.Join(user, "latexmk"), 0700); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{
		"resourceCache": map[string]any{"directory": t.TempDir(), "maxBytes": 12345, "acquisition": "client"},
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(user, "latexmk", UserFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	environment := t.TempDir()
	t.Setenv("LATEXMK_CLI_RESOURCE_CACHE_DIR", environment)
	t.Setenv("LATEXMK_CLI_RESOURCE_CACHE_BYTES", "54321")
	t.Setenv("LATEXMK_RESOURCE_ACQUISITION", "server")
	cfg, err := Load(t.TempDir(), CredentialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(environment)
	if cfg.ResourceCache.Directory != canonical || cfg.ResourceCache.MaxBytes != 54321 ||
		!slices.Contains(cfg.DenyFiles, canonical) || cfg.ResourceCache.Acquisition != "server" {
		t.Fatalf("cache settings precedence or exclusion failed: %#v", cfg.ResourceCache)
	}
}

func TestResourceAcquisitionDefaultsAndValidation(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("LATEXMK_RESOURCE_ACQUISITION", "")
	cfg, err := Load(t.TempDir(), CredentialOptions{})
	if err != nil || cfg.ResourceCache.Acquisition != "auto" {
		t.Fatalf("unexpected default acquisition: %s %v", cfg.ResourceCache.Acquisition, err)
	}
	t.Setenv("LATEXMK_RESOURCE_ACQUISITION", "unknown")
	if _, err := Load(t.TempDir(), CredentialOptions{}); err == nil {
		t.Fatal("invalid acquisition mode accepted")
	}
}

func TestResourceCacheRejectsInvalidEnvironmentAndRelativePaths(t *testing.T) {
	for _, value := range []string{"-1", "0", "1MiB", "1099511627777"} {
		t.Run(value, func(t *testing.T) {
			isolateUserConfig(t)
			t.Setenv("LATEXMK_CLI_RESOURCE_CACHE_BYTES", value)
			if _, err := Load(t.TempDir(), CredentialOptions{}); err == nil {
				t.Fatal("invalid cache limit accepted")
			}
		})
	}
	isolateUserConfig(t)
	t.Setenv("LATEXMK_CLI_RESOURCE_CACHE_DIR", "relative/cache")
	if _, err := Load(t.TempDir(), CredentialOptions{}); err == nil {
		t.Fatal("relative cache directory accepted")
	}
}
