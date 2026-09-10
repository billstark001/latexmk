package compile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceResetAndClose(t *testing.T) {
	w, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Project, "old.aux"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.Project, "linked")); err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(w.Path, "result.tar.gz"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Reset(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(w.Project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("reset: %v %v", entries, err)
	}
	for _, path := range []string{filepath.Join(outside, "keep"), filepath.Join(w.Path, "result.tar.gz")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("removed unrelated file: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace leaked: %v", err)
	}
	if err := w.Reset(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reset after close: %v", err)
	}
}
