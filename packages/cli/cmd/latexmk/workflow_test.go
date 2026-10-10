package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestExplicitEngineOverridesTargetForSelectionAndPack(t *testing.T) {
	for _, test := range []struct {
		name, graphic string
		args          []string
	}{
		{"pdf flag", "plot.png", []string{"latexmk", "files", "-pdf", "--json"}},
		{"pdflatex flag", "plot.png", []string{"latexmk", "files", "-pdflatex", "--json"}},
		{"xelatex flag", "plot.PDF", []string{"latexmk", "files", "-xelatex", "--json"}},
		{"pdfxe flag", "plot.PDF", []string{"latexmk", "files", "-pdfxe", "--json"}},
		{"named engine", "plot.png", []string{"latexmk", "files", "--engine=pdflatex", "--json"}},
		{"last engine wins", "plot.PDF", []string{"latexmk", "files", "-pdf", "-xelatex", "--json"}},
		{"pdflatex executable", "plot.png", []string{"pdflatex", "--dry-run", "--json"}},
		{"pack pdf flag", "plot.png", []string{"latexmk", "pack", "-pdf", "--dry-run", "--json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			for name, data := range map[string]string{
				".latexmk.json": `{"targets":{"paper":{"entry":"main.tex","engine":"lualatex"}}}`,
				"main.tex":      `\includegraphics{plot}`,
				"plot.png":      "PNG",
				"plot.PDF":      "PDF",
			} {
				if err := os.WriteFile(name, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := captureCommandOutput(t, func() int { return run(test.args) })
			var response struct {
				Files []struct{ Path string }
				Data  struct{ Files []struct{ Path string } }
			}
			if err := json.Unmarshal([]byte(stdout), &response); err != nil || code != 0 {
				t.Fatalf("preview: %d %s %s %v", code, stdout, stderr, err)
			}
			files := response.Files
			if response.Data.Files != nil {
				files = response.Data.Files
			}
			if len(files) != 2 || files[1].Path != test.graphic {
				t.Fatalf("selected %v, want main.tex and %s", files, test.graphic)
			}
		})
	}
}

func TestImplicitTargetSelection(t *testing.T) {
	tests := []struct {
		name, config, args, entry, errorText string
	}{
		{"sole target", `{"targets":{"paper":{"entry":"paper.tex"}}}`, "", "paper.tex", ""},
		{
			"configured default",
			`{"defaultTarget":"b","targets":{"a":{"entry":"a.tex"},"b":{"entry":"b.tex"}}}`,
			"",
			"b.tex",
			"",
		},
		{
			"ambiguous",
			`{"targets":{"b":{"entry":"b.tex"},"a":{"entry":"a.tex"}}}`,
			"",
			"",
			"multiple build targets configured (a, b)",
		},
		{
			"missing default",
			`{"defaultTarget":"other","targets":{"paper":{"entry":"paper.tex"}}}`,
			"",
			"",
			`defaultTarget "other" is not a configured target`,
		},
		{
			"reserved default",
			`{"defaultTarget":"all","targets":{"paper":{"entry":"paper.tex"}}}`,
			"",
			"",
			`defaultTarget cannot be the reserved target name "all"`,
		},
		{
			"empty explicit target",
			`{"targets":{"paper":{"entry":"paper.tex"}}}`,
			"--target=",
			"",
			"--target requires a nonempty name",
		},
		{
			"reserved target",
			`{"targets":{"all":{"entry":"paper.tex"}}}`,
			"--target all",
			"",
			`target name "all" is reserved`,
		},
		{
			"explicit target",
			`{"defaultTarget":"b","targets":{"a":{"entry":"a.tex"},"b":{"entry":"b.tex"}}}`,
			"--target a",
			"a.tex",
			"",
		},
		{"explicit entry", `{"defaultTarget":"other","targets":{"paper":{"entry":"paper.tex"}}}`, "a.tex", "a.tex", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			for name, data := range map[string]string{
				".latexmk.json": tc.config,
				"a.tex":         "a", "b.tex": "b", "paper.tex": "paper",
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"latexmk", "files", "--json"}
			if tc.args != "" {
				args = append(args, strings.Fields(tc.args)...)
			}
			status, stdout, stderr := captureCommandOutput(t, func() int { return run(args) })
			if tc.errorText != "" {
				if status == 0 || !strings.Contains(stderr, tc.errorText) {
					t.Fatalf("status %d, stderr %q; want %q", status, stderr, tc.errorText)
				}
				return
			}
			if status != 0 {
				t.Fatalf("status %d, stderr %q", status, stderr)
			}
			var view manifestView
			if err := json.Unmarshal([]byte(stdout), &view); err != nil {
				t.Fatal(err)
			}
			if view.Entry != tc.entry {
				t.Fatalf("entry = %q, want %q", view.Entry, tc.entry)
			}
		})
	}
}

func TestTargetsPreviewPreservesFlagsAndNeedsNoCredentials(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LATEXMK_TOKEN", "")
	t.Setenv("LATEXMK_TOKEN_FILE", "missing-token")
	for name, data := range map[string]string{
		".latexmk.json": `{"projectRoot":".","uploadMode":"manifest","includeFiles":["*.tex"],"targets":{"a":{"entry":"a.tex"},"b":{"entry":"b.tex"}}}`,
		"a.tex":         "a", "b.tex": "b",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if status := run(
		[]string{"latexmk", "files", "--target", "all", "--token-file", "also-missing", "--json"},
	); status != 0 {
		t.Fatalf("target preview status %d", status)
	}
	if _, err := os.Stat(filepath.Join(root, ".latexmk-cache")); !os.IsNotExist(err) {
		t.Fatal("preview created local state")
	}
	if status := run([]string{"latexmk", "files", "--target", "all", "--watch"}); status == 0 {
		t.Fatal("all accepted watch")
	}
}

func TestTargetPDFExport(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.pdf"), []byte("pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{projectRoot: root, outDir: root, entry: "main.tex", pdfExport: "exports/paper.pdf"}
	digest := sha256.Sum256([]byte("pdf"))
	out := client.CompileOutput{
		Result: protocol.CompileResult{
			Success:   true,
			Artifacts: []protocol.Artifact{{Path: "main.pdf", Size: 3, SHA256: hex.EncodeToString(digest[:])}},
		},
	}
	if err := exportPDF(opts, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "exports/paper.pdf"))
	if err != nil || string(data) != "pdf" {
		t.Fatalf("PDF export: %v", err)
	}
	opts.pdfExport = "main.tex"
	if err := exportPDF(opts, out); err == nil {
		t.Fatal("export accepted a source extension")
	}
	opts.pdfExport = "../escape.pdf"
	if err := exportPDF(opts, out); err == nil {
		t.Fatal("export escaped project root")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err == nil {
		opts.pdfExport = "linked/escape.pdf"
		if err := exportPDF(opts, out); err == nil {
			t.Fatal("export followed a symlink parent")
		}
		if _, err := os.Stat(filepath.Join(outside, "escape.pdf")); !os.IsNotExist(err) {
			t.Fatal("export wrote outside project")
		}
	}
	opts.pdfExport = "exports/paper.pdf"
	out.Result.Artifacts = append(out.Result.Artifacts, protocol.Artifact{Path: "other/main.pdf"})
	if err := exportPDF(opts, out); err == nil {
		t.Fatal("ambiguous PDF silently chosen")
	}
}

func TestCacheCleanPreservesProjectIdentity(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(root, ".latexmk-cache/aux/entry"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".latexmk-cache/project-id"), []byte("identity"), 0600); err != nil {
		t.Fatal(err)
	}
	if status := run([]string{"latexmk", "cache", "clean"}); status != 0 {
		t.Fatalf("cache clean: %d", status)
	}
	if _, err := os.Stat(filepath.Join(root, ".latexmk-cache/aux")); !os.IsNotExist(err) {
		t.Fatal("auxiliary cache survived")
	}
	if _, err := os.Stat(filepath.Join(root, ".latexmk-cache/project-id")); err != nil {
		t.Fatal("project identity removed")
	}
}
