package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeSourceFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestServerValueSources(t *testing.T) {
	for _, declaration := range []string{
		`"https://example.test"`,
		`{"value":"https://example.test"}`,
		`{"env":"TEST_SERVER_SOURCE"}`,
		`{"file":"address.txt"}`,
	} {
		t.Run(declaration, func(t *testing.T) {
			isolateUserConfig(t)
			t.Setenv("LATEXMK_SERVER", "")
			t.Setenv("TEST_SERVER_SOURCE", " https://example.test\n")
			root := t.TempDir()
			writeSourceFixture(t, root, FileName, `{"server":`+declaration+`}`)
			writeSourceFixture(t, root, "address.txt", " https://example.test\n")
			cfg, err := Load(root)
			if err != nil || cfg.Server != "https://example.test" {
				t.Fatalf("server = %q, error = %v", cfg.Server, err)
			}
		})
	}
}

func TestSourceErrorsAreRedacted(t *testing.T) {
	for index, test := range []struct{ declaration, message string }{
		{`"token":"DO_NOT_EXPOSE"`, "hardcoded"},
		{`"token":{"value":"DO_NOT_EXPOSE"}`, "hardcoded"},
		{`"token":{"unknown":"DO_NOT_EXPOSE"}`, "source must"},
		{`"token":{"env":"TEST_MISSING_SOURCE"}`, "unset"},
		{`"server":{"env":"TEST_MISSING_SOURCE"}`, "unset"},
		{`"server":{"env":"TEST_EMPTY_SOURCE"}`, "empty"},
		{`"token":{"env":"TEST_EMPTY_SOURCE"}`, "empty"},
		{`"server":""`, "empty"},
		{`"server":{"file":"missing"}`, "read server file"},
		{`"token":{"file":"missing"}`, "read token file"},
		{`"server":{"file":"empty"}`, "empty"},
		{`"token":{"file":"empty"}`, "empty"},
		{`"server":{"file":"directory"}`, "regular file"},
		{`"server":{"file":"multiline"}`, "exactly one"},
		{`"server":{"file":"oversize"}`, "exceeds"},
		{`"server":{"env":"TEST_EMPTY_SOURCE","value":"DO_NOT_EXPOSE"}`, "exactly one"},
		{`"server":null`, "exactly one"},
		{`"token":{"env":null}`, "nonempty"},
		{`"token":{"file":"missing"},"tokenFile":"other"`, "mutually exclusive"},
	} {
		t.Run(fmt.Sprintf("case_%d", index), func(t *testing.T) {
			isolateUserConfig(t)
			t.Setenv("LATEXMK_SERVER", "")
			t.Setenv("TEST_EMPTY_SOURCE", " \n")
			t.Setenv("TEST_MISSING_SOURCE", "")
			if err := os.Unsetenv("TEST_MISSING_SOURCE"); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeSourceFixture(t, root, FileName, "{"+test.declaration+"}")
			writeSourceFixture(t, root, "empty", " \n")
			writeSourceFixture(t, root, "multiline", "DO_NOT_EXPOSE\nsecond")
			writeSourceFixture(t, root, "oversize", strings.Repeat("x", maxTokenFileSize+1))
			if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
				t.Fatal(err)
			}
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), test.message) ||
				strings.Contains(err.Error(), "DO_NOT_EXPOSE") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDeclaredSourceLayeringAndDotenv(t *testing.T) {
	userDir := filepath.Join(isolateUserConfig(t), "latexmk")
	t.Setenv("LATEXMK_SERVER", "")
	writeSourceFixture(t, userDir, UserFileName, `{"server":{"file":"address"},"token":{"file":"credential"}}`)
	writeSourceFixture(t, userDir, "address", "https://user.test")
	writeSourceFixture(t, userDir, "credential", "user-secret")
	root := t.TempDir()
	writeSourceFixture(t, root, FileName, `{"token":{"file":"missing"}}`)
	cfg, err := Load(root)
	if err != nil || cfg.Server != "https://user.test" || cfg.Token != "user-secret" {
		t.Fatalf("user source resolution failed: %v", err)
	}
	writeSourceFixture(t, root, FileName, `{"server":{"env":"DECLARED_SERVER"},"token":{"file":"missing"}}`)
	writeSourceFixture(t, root, EnvFileName, "DECLARED_SERVER=https://dotenv.test\n")
	cfg, err = Load(root)
	if err != nil || cfg.Server != "https://dotenv.test" {
		t.Fatalf("dotenv source failed: %v", err)
	}
	t.Setenv("DECLARED_SERVER", "https://process.test")
	cfg, err = Load(root)
	if err != nil || cfg.Server != "https://process.test" {
		t.Fatalf("process source failed: %v", err)
	}
	t.Setenv("LATEXMK_SERVER", "https://override.test")
	writeSourceFixture(t, root, FileName, `{"server":{"file":"missing"}}`)
	cfg, err = Load(root)
	if err != nil || cfg.Server != "https://override.test" {
		t.Fatalf("server override opened unused file: %v", err)
	}
}

func TestDeclaredCredentialLazyResolutionAndDeny(t *testing.T) {
	isolateUserConfig(t)
	root := t.TempDir()
	writeSourceFixture(t, root, FileName, `{"token":{"file":"custom-secret"}}`)
	cfg, _, err := LoadArgs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.DenyFiles, filepath.Join(root, "custom-secret")) {
		t.Fatal("preview must deny the declared credential file")
	}
	if err := cfg.Authenticate(root); err == nil {
		t.Fatal("missing explicit credential accepted")
	}
	writeSourceFixture(t, root, "custom-secret", "file-secret\n")
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "file-secret" {
		t.Fatalf("file source failed: %v", err)
	}
	cfg.TokenMode = "file"
	if err := cfg.Authenticate(root); err != nil || cfg.Token != "file-secret" {
		t.Fatalf("file mode failed: %v", err)
	}
	writeSourceFixture(t, root, FileName, `{"token":{"env":"DECLARED_TOKEN"}}`)
	writeSourceFixture(t, root, EnvFileName, "DECLARED_TOKEN=dotenv-secret\n")
	cfg, err = Load(root)
	if err != nil || cfg.Token != "dotenv-secret" {
		t.Fatalf("token dotenv source failed: %v", err)
	}
	t.Setenv("DECLARED_TOKEN", "process-secret")
	cfg, err = Load(root)
	if err != nil || cfg.Token != "process-secret" {
		t.Fatalf("token process source failed: %v", err)
	}
	writeSourceFixture(t, root, FileName, `{"token":{"file":"missing"}}`)
	for _, args := range [][]string{{"--token", "cli-secret"}, {"--token-mode", "none"}} {
		cfg, _, err = LoadArgs(root, args)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Authenticate(root); err != nil {
			t.Fatalf("opened unused token file: %v", err)
		}
	}
}

func TestWriteRejectsHardcodedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	err := Write(path, FileConfig{Token: "DO_NOT_EXPOSE"})
	if err == nil || strings.Contains(err.Error(), "DO_NOT_EXPOSE") {
		t.Fatalf("unsafe write: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("wrote forbidden credential")
	}
}
