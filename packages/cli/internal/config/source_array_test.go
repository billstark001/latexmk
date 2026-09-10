package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerSourcesNormalizeAndRoundTrip(t *testing.T) {
	for index, test := range []struct{ input, normalized string }{
		{`"https://literal.test"`, `[{"value":"https://literal.test"}]`},
		{`{"env":"SERVER_NAME"}`, `[{"env":"SERVER_NAME"}]`},
		{`{"file":"address"}`, `[{"file":"address"}]`},
		{`[{"env":"SERVER_NAME"},{"file":"address"},"https://literal.test"]`,
			`[{"env":"SERVER_NAME"},{"file":"address"},{"value":"https://literal.test"}]`},
		{`["",{"value":" "},"https://literal.test"]`,
			`[{"value":""},{"value":" "},{"value":"https://literal.test"}]`},
	} {
		t.Run(fmt.Sprintf("case_%d", index), func(t *testing.T) {
			var cfg FileConfig
			if err := json.Unmarshal([]byte(`{"server":`+test.input+`}`), &cfg); err != nil {
				t.Fatal(err)
			}
			output, err := json.Marshal(cfg.Server)
			if err != nil || string(output) != test.normalized {
				t.Fatalf("normalized = %s, error = %v", output, err)
			}
			path := filepath.Join(t.TempDir(), FileName)
			if err := Write(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var reread FileConfig
			if err := json.Unmarshal(data, &reread); err != nil {
				t.Fatal(err)
			}
			output, err = json.Marshal(reread.Server)
			if err != nil || string(output) != test.normalized {
				t.Fatalf("round trip = %s, error = %v", output, err)
			}
		})
	}
}

func TestServerArrayResolution(t *testing.T) {
	for index, test := range []struct{ input, want, errorText string }{
		{`[{"env":"ARRAY_SERVER"},{"file":"address"},"https://literal.test"]`, "https://env.test", ""},
		{`[{"file":"address"},{"env":"ARRAY_SERVER"}]`, "https://file.test", ""},
		{`["https://literal.test",{"file":"directory"}]`, "https://literal.test", ""},
		{`[{"env":"MISSING_ARRAY_SERVER"},{"file":"missing"},{"file":"address"}]`, "https://file.test", ""},
		{`["",{"value":" \n"},{"env":"EMPTY_ARRAY_SERVER"},{"file":"empty"},"https://literal.test"]`,
			"https://literal.test", ""},
		{`[{"env":"MISSING_ARRAY_SERVER"},{"file":"missing"},""]`, "", "no nonempty available source"},
		{`{"value":""}`, "", "no nonempty available source"},
		{`" \n"`, "", "no nonempty available source"},
		{`[{"file":"directory"},"https://literal.test"]`, "", "regular file"},
		{`[{"file":"multiline"},"https://literal.test"]`, "", "exactly one"},
		{`[{"file":"oversize"},"https://literal.test"]`, "", "exceeds"},
		{`[{"env":"MULTILINE_ARRAY_SERVER"},"https://literal.test"]`, "", "exactly one"},
	} {
		t.Run(fmt.Sprintf("case_%d", index), func(t *testing.T) {
			isolateUserConfig(t)
			t.Setenv("LATEXMK_SERVER", "")
			t.Setenv("ARRAY_SERVER", " https://env.test\n")
			t.Setenv("EMPTY_ARRAY_SERVER", " \t\n")
			t.Setenv("MULTILINE_ARRAY_SERVER", "DO_NOT_EXPOSE\nsecond")
			t.Setenv("MISSING_ARRAY_SERVER", "")
			if err := os.Unsetenv("MISSING_ARRAY_SERVER"); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			writeSourceFixture(t, root, FileName, `{"server":`+test.input+`}`)
			writeSourceFixture(t, root, "address", " https://file.test\n")
			writeSourceFixture(t, root, "empty", "\n")
			writeSourceFixture(t, root, "multiline", "DO_NOT_EXPOSE\nsecond")
			writeSourceFixture(t, root, "oversize", strings.Repeat("x", maxTokenFileSize+1))
			if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(root)
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) ||
					strings.Contains(err.Error(), "DO_NOT_EXPOSE") {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err != nil || cfg.Server != test.want {
				t.Fatalf("server = %q, error = %v", cfg.Server, err)
			}
		})
	}
}

func TestServerRejectsInvalidArrayDeclarationsEvenWhenOverridden(t *testing.T) {
	for index, input := range []string{
		`null`, `[]`, `[null]`, `[[]]`, `[42]`, `true`, `{}`,
		`[{"env":""}]`, `[{"file":" "}]`, `[{"value":null}]`,
		`[{"env":"NAME","file":"address"}]`,
		`["https://first.test",{"unknown":"DO_NOT_EXPOSE"}]`,
	} {
		t.Run(fmt.Sprintf("case_%d", index), func(t *testing.T) {
			isolateUserConfig(t)
			t.Setenv("LATEXMK_SERVER", "https://override.test")
			root := t.TempDir()
			writeSourceFixture(t, root, FileName, `{"server":`+input+`}`)
			if _, err := Load(root); err == nil || strings.Contains(err.Error(), "DO_NOT_EXPOSE") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestServerArrayLayeringAndEmptyEnvironment(t *testing.T) {
	userDir := filepath.Join(isolateUserConfig(t), "latexmk")
	t.Setenv("LATEXMK_SERVER", "")
	t.Setenv("ARRAY_SERVER", " ")
	writeSourceFixture(t, userDir, UserFileName, `{"server":[{"file":"missing"},{"file":"address"}]}`)
	writeSourceFixture(t, userDir, "address", "https://user.test")
	root := t.TempDir()
	writeSourceFixture(t, root, FileName, `{"engine":"xelatex"}`)
	cfg, err := Load(root)
	if err != nil || cfg.Server != "https://user.test" {
		t.Fatalf("inherited file paths: %v", err)
	}
	writeSourceFixture(t, root, FileName, `{"server":[{"env":"ARRAY_SERVER"},{"file":"address"}]}`)
	writeSourceFixture(t, root, EnvFileName, "ARRAY_SERVER=https://dotenv.test\n")
	writeSourceFixture(t, root, "address", "https://project.test")
	cfg, err = Load(root)
	if err != nil || cfg.Server != "https://project.test" {
		t.Fatalf("empty process value must mask dotenv and try next source: %v", err)
	}
	t.Setenv("LATEXMK_SERVER", " \t")
	cfg, err = Load(root)
	if err != nil || cfg.Server != "https://project.test" {
		t.Fatalf("blank override must use configured sources: %v", err)
	}
	writeSourceFixture(t, root, FileName, `{"server":[{"file":"missing"}]}`)
	if _, err := Load(root); err == nil {
		t.Fatal("project sources must replace rather than append user sources/defaults")
	}
	t.Setenv("LATEXMK_SERVER", " https://override.test\n")
	cfg, err = Load(root)
	if err != nil || cfg.Server != "https://override.test" {
		t.Fatalf("environment override failed: %v", err)
	}
}

func TestServerArrayDoesNotHidePermissionErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without read permissions")
	}
	isolateUserConfig(t)
	t.Setenv("LATEXMK_SERVER", "")
	root := t.TempDir()
	writeSourceFixture(t, root, FileName, `{"server":[{"file":"unreadable"},"https://fallback.test"]}`)
	path := filepath.Join(root, "unreadable")
	writeSourceFixture(t, root, "unreadable", "https://file.test")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unreadable file should fail immediately: %v", err)
	}
}

func TestWriteDefaultServerAndRejectEmptyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := Write(path, FileConfig{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Server) != 1 || cfg.Server[0].Value == nil || *cfg.Server[0].Value != "http://127.0.0.1:8080" {
		t.Fatalf("unexpected default source: %+v", cfg.Server)
	}
	for _, sources := range []ServerSources{{}, {{}}, {{Env: "NAME", File: "file"}}} {
		if err := Write(path, FileConfig{Server: sources}); err == nil {
			t.Fatal("invalid source list was written")
		}
	}
}
