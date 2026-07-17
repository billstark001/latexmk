package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/config"
)

func TestValidateRejectsTraversal(t *testing.T) {
	r := NewRunner(config.Config{Engines: []string{"xelatex"}})
	err := r.Validate(t.TempDir(), api.CompileRequest{ProtocolVersion: 1, Entry: "../main.tex", Engine: "xelatex", Interaction: "nonstopmode"})
	if err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestSandboxEnvironmentDoesNotInheritHostSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-reach-tex")
	env := strings.Join(sandboxEnvironment(t.TempDir(), false), "\n")
	if strings.Contains(env, "AWS_SECRET_ACCESS_KEY") || strings.Contains(env, "must-not-reach-tex") {
		t.Fatal("compile environment inherited a host secret")
	}
	if !strings.Contains(env, "PATH=/usr/local/bin:/usr/bin:/bin") || !strings.Contains(env, "shell_escape=f") {
		t.Fatalf("unexpected sandbox environment: %s", env)
	}
}

func TestValidateAcceptsEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(config.Config{Engines: []string{"xelatex"}})
	err := r.Validate(root, api.CompileRequest{ProtocolVersion: 1, Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidJobName(t *testing.T) {
	if !validJobName("paper-v1.2") || validJobName("../paper") || validJobName("") {
		t.Fatal("job name validation failed")
	}
}

func TestCommandArgsHardensLuaLaTeX(t *testing.T) {
	args := commandArgs(api.CompileRequest{
		Entry:       "main.tex",
		Engine:      "lualatex",
		Interaction: "nonstopmode",
	})
	joined := strings.Join(args, "\n")
	for _, required := range []string{
		"-lualatex",
		"-pdflualatex=lualatex --safer --nosocket %O %S",
		"-no-shell-escape",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("LuaLaTeX args missing %q: %v", required, args)
		}
	}
}

func TestCommandArgsDoesNotAddLuaOptionsToOtherEngines(t *testing.T) {
	for _, engine := range []string{"xelatex", "pdflatex"} {
		args := commandArgs(api.CompileRequest{
			Entry:       "main.tex",
			Engine:      engine,
			Interaction: "nonstopmode",
		})
		joined := strings.Join(args, "\n")
		if strings.Contains(joined, "--safer") || strings.Contains(joined, "--nosocket") {
			t.Errorf("%s unexpectedly received Lua options: %v", engine, args)
		}
	}
}

func TestCollectArtifactsIncludesXdvipdfmxPDF(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"main.fls": "OUTPUT main.xdv\n",
		"main.xdv": "xdv",
		"main.pdf": "%PDF-test",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, err := collectArtifacts(root, api.CompileRequest{Entry: "main.tex", Engine: "xelatex"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	foundPDF := false
	for _, artifact := range artifacts {
		if artifact.RelativePath == "main.pdf" {
			foundPDF = true
		}
	}
	if !foundPDF {
		t.Fatal("main.pdf was not collected")
	}
}
