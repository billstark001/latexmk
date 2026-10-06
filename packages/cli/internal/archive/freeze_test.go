package archive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFreezeKeepsHashedBytesAfterEditorReplacement(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.tex")
	if err := os.WriteFile(source, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	frozen, err := Freeze(context.Background(), []File{{Path: "main.tex", Source: source}}, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = frozen.Close() }()
	if err := os.WriteFile(source, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(frozen.Files[0], 5)
	if err != nil || string(data) != "first" || frozen.Files[0].SHA256 == "" {
		t.Fatalf("snapshot=%q %v", data, err)
	}
	if _, err := Freeze(context.Background(), []File{{Path: "main.tex", Source: source}}, 1, 4); err == nil {
		t.Fatal("byte quota was ignored")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "secret"), source); err != nil {
		t.Fatal(err)
	}
	if _, err := Freeze(context.Background(), []File{{Path: "main.tex", Source: source}}, 1, 5); err == nil {
		t.Fatal("symlink was captured")
	}
}
