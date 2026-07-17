package dependency

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadExplicitManifestReadsExactPaths(t *testing.T) {
	root := t.TempDir()
	content := "# Dynamic dependencies\nsections/body.tex\n\n figures/plot.pdf \nsections/body.tex\n"
	if err := os.WriteFile(filepath.Join(root, ".latexmk-files"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := LoadExplicitManifest(root, ".latexmk-files")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != "figures/plot.pdf" || files[1] != "sections/body.tex" {
		t.Fatalf("manifest files = %#v", files)
	}
}

func TestLoadExplicitManifestRejectsOutsideEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".latexmk-files"), []byte("../secret.tex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExplicitManifest(root, ".latexmk-files"); err == nil {
		t.Fatal("expected out-of-root manifest entry to fail")
	}
}

func TestNormalizeExplicitManifestPathRejectsGlob(t *testing.T) {
	if _, err := NormalizeExplicitManifestPath("policy/*.txt"); err == nil {
		t.Fatal("expected glob manifest path to fail")
	}
}

func TestLoadExplicitManifestRejectsSymlinkPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "files.txt")
	if err := os.WriteFile(outside, []byte("secret.tex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".latexmk-files")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExplicitManifest(root, ".latexmk-files"); err == nil {
		t.Fatal("expected symlink manifest to fail")
	}
}
