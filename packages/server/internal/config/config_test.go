package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/engine"
)

var customEngineCounter atomic.Uint64

func TestEngineConfigurationUsesExactRegisteredNames(t *testing.T) {
	name := fmt.Sprintf("Lab/Custom TeX+%d", customEngineCounter.Add(1))
	driver, err := engine.Default.Lookup("pdflatex")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Default.Register(name, driver); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	t.Setenv("LATEXMK_ENGINES", " "+name+", "+name+" ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Engines) != 1 || cfg.Engines[0] != name || !cfg.EngineAllowed(name) {
		t.Fatalf("engine names were normalized: %v", cfg.Engines)
	}
	for _, unregistered := range []string{strings.ToLower(name), "unknown-engine"} {
		t.Setenv("LATEXMK_ENGINES", unregistered)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unregistered engine") {
			t.Fatalf("unregistered engine %q: %v", unregistered, err)
		}
	}
}

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

func TestCompileCacheLimitCanDisableFeature(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	t.Setenv("LATEXMK_MAX_COMPILE_CACHE_BYTES", "0")
	cfg, err := Load()
	if err != nil || cfg.MaxCompileCacheBytes != 0 {
		t.Fatalf("disabled cache: %d %v", cfg.MaxCompileCacheBytes, err)
	}
	t.Setenv("LATEXMK_MAX_COMPILE_CACHE_BYTES", "2MiB")
	t.Setenv("LATEXMK_COMPILE_CACHE_RETENTION", "2h")
	cfg, err = Load()
	if err != nil || cfg.MaxCompileCacheBytes != 2<<20 || cfg.CompileCacheRetention.Hours() != 2 {
		t.Fatalf("cache limits: %+v %v", cfg, err)
	}
}

func TestRealtimeSessionsCanBeDisabled(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	t.Setenv("LATEXMK_MAX_REALTIME_SESSIONS", "0")
	cfg, err := Load()
	if err != nil || cfg.MaxRealtimeSessions != 0 || cfg.MaxRealtimeSessionsPerOwner <= 0 {
		t.Fatalf("sessions=%+v %v", cfg, err)
	}
}

func TestRunnerRejectsNativeLegacyAndShellEscapeConfiguration(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	t.Setenv("LATEXMK_RUNNER_IMAGE", "test@sha256:"+strings.Repeat("a", 64))
	t.Setenv("LATEXMK_RUNNER_NAMESPACE", "isolated-test")
	t.Setenv("LATEXMK_ENABLE_LEGACY_COMPILE", "true")
	if _, err := Load(); err == nil {
		t.Fatal("native legacy route was accepted alongside daemon access")
	}
	t.Setenv("LATEXMK_ENABLE_LEGACY_COMPILE", "false")
	t.Setenv("LATEXMK_ALLOW_SHELL_ESCAPE", "true")
	if _, err := Load(); err == nil {
		t.Fatal("shell escape was accepted alongside isolated runner")
	}
}

func TestDerivedResourceLimitsRejectOverflow(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	for _, name := range []string{"LATEXMK_MAX_COMPILE_CACHE_BYTES", "LATEXMK_RUNNER_WORKSPACE_BYTES"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "9223372036854775807")
			if _, err := Load(); err == nil {
				t.Fatal("derived byte budget can overflow")
			}
		})
	}
}

func TestValidateChecksRunnerSettingsWithoutLoad(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"mutable image", func(c *Config) { c.RunnerImage = "image:latest"; c.RunnerNamespace = "test-namespace" }},
		{"missing namespace", func(c *Config) { c.RunnerImage = "image@sha256:" + strings.Repeat("a", 64); c.RunnerNamespace = "" }},
		{"workspace budget", func(c *Config) { c.RunnerWorkspaceBytes = 0 }},
		{"revision rate", func(c *Config) { c.MaxRealtimeRevisionRate = 0 }},
		{"owner sessions", func(c *Config) { c.MaxRealtimeSessionsPerOwner = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := cfg
			test.mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatal("invalid runner settings accepted")
			}
		})
	}
}

func TestTokenAuthRejectsUnusableCredentials(t *testing.T) {
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	t.Setenv("LATEXMK_API_TOKEN", "a-valid-token-with-enough-characters")
	t.Setenv("LATEXMK_BOOTSTRAP_TOKEN", "a-valid-token-with-enough-characters")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	for _, mode := range []string{"token", "postgres"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("LATEXMK_AUTH_MODE", mode)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			for _, token := range []string{strings.Repeat(" ", 24), "long-enough-token-value with-space", "long-enough-token-value\x00control"} {
				cfg.APIToken, cfg.BootstrapToken = token, token
				if err := cfg.Validate(); err == nil {
					t.Fatal("unusable credential accepted")
				}
			}
		})
	}
}

func TestDerivedCountLimitsRejectOverflow(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.MaxQueuedJobs = math.MaxInt }, func(c *Config) { c.MaxFiles = math.MaxInt }} {
		bad := cfg
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("accepted an overflowing derived count")
		}
	}
}

func TestAPITokenFileSupportsSecretSymlinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("a-secure-token-value-at-least-24-characters\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "secret")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("LATEXMK_API_TOKEN", "")
	t.Setenv("LATEXMK_API_TOKEN_FILE", link)
	if token, err := loadAPIToken(); err != nil || token != "a-secure-token-value-at-least-24-characters" {
		t.Fatalf("secret symlink: %q %v", token, err)
	}
}
