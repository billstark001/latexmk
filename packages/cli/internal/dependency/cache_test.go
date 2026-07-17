package dependency

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDependencyCacheRoundTripIsScopedByEntryAndEngine(t *testing.T) {
	root := t.TempDir()
	if err := SaveCachedInputs(root, "main.tex", "xelatex", []string{"main.tex", "sections/body.tex", "main.tex"}); err != nil {
		t.Fatal(err)
	}
	inputs, found, err := LoadCachedInputs(root, "main.tex", "xelatex")
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(inputs) != 2 || inputs[0] != "main.tex" || inputs[1] != "sections/body.tex" {
		t.Fatalf("cached inputs = %#v, found=%t", inputs, found)
	}
	if _, found, err := LoadCachedInputs(root, "main.tex", "lualatex"); err != nil || found {
		t.Fatalf("unexpected cache for another engine: found=%t err=%v", found, err)
	}
	info, err := os.Stat(filepath.Join(root, cacheDirName, cacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %o", info.Mode().Perm())
	}
}

func TestDependencyCacheRejectsSymlinkDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, cacheDirName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCachedInputs(root, "main.tex", "xelatex"); err == nil {
		t.Fatal("expected symlink cache directory read to be rejected")
	}
	if err := SaveCachedInputs(root, "main.tex", "xelatex", []string{"main.tex"}); err == nil {
		t.Fatal("expected symlink cache directory to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, cacheFileName)); !os.IsNotExist(err) {
		t.Fatalf("cache escaped through symlink: %v", err)
	}
}

func TestDependencyCacheRejectsOutsidePaths(t *testing.T) {
	if err := SaveCachedInputs(t.TempDir(), "main.tex", "xelatex", []string{"../secret.tex"}); err == nil {
		t.Fatal("expected outside cache path to be rejected")
	}
}
