//go:build !windows

package safefs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	root := testRoot(t)
	if err := syscall.Mkfifo(filepath.Join(root.Name(), "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := root.OpenRegular("pipe"); err == nil {
		_ = f.Close()
		t.Fatal("opened FIFO")
	}
}

func TestRootRemainsAnchoredAcrossDirectoryReplacement(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "root")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteExclusive("data", 10, textWriter("original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), original); err != nil {
		t.Fatal(err)
	}
	if data, err := root.ReadLimited("data", 10); err != nil || string(data) != "original" {
		t.Fatalf("root lost confinement: %q %v", data, err)
	}
	if _, err := root.WriteAtomic("data", 10, textWriter("updated")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(parent, "moved", "data"))
	if err != nil || string(data) != "updated" {
		t.Fatalf("write lost confinement: %q %v", data, err)
	}
}
