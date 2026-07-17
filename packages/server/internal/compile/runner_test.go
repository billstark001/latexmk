package compile

import (
	"fmt"
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

func TestCollectRecordedInputsOnlyReturnsWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.tex")
	for name, content := range map[string]string{
		"main.tex":          "main",
		"sections/body.tex": "body",
		"main.aux":          "generated",
	} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	fls := fmt.Sprintf("PWD %s\nINPUT main.tex\nINPUT sections/body.tex\nINPUT main.aux\nINPUT %s\nINPUT /usr/share/texmf/system.sty\n", root, outside)
	if err := os.WriteFile(filepath.Join(root, "main.fls"), []byte(fls), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs, err := collectRecordedInputs(root)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(inputs, ",")
	if got != "main.aux,main.tex,sections/body.tex" {
		t.Fatalf("recorded inputs = %q", got)
	}
}
