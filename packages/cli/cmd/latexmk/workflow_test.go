package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

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
