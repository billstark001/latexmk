package archive

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func TestSelectionExcludesOnlyPrivateStagedNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"main.tex", ".latexmk-custom"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := safefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fs.Close() }()
	pending, err := fs.Stage("paper.pdf", 3, func(w io.Writer) error { _, err := io.WriteString(w, "pdf"); return err })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pending.Close() }()
	files, _, err := Manifest(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("selected private stage or excluded user file: %v", files)
	}
	for _, file := range files {
		if safefs.IsStagedName(filepath.Base(file.Path)) {
			t.Fatal("selected private stage", file.Path)
		}
	}
}
