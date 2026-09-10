package archive

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedFileReplacedBySymlinkIsNotRead(t *testing.T) {
	for _, replaceParent := range []bool{false, true} {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "sections/main.tex"), "source")
		files, _, err := Manifest(Options{Root: root, DeferHash: true})
		if err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		mustWrite(t, filepath.Join(outside, "main.tex"), "private content")
		name, target := filepath.Join(root, "sections/main.tex"), filepath.Join(outside, "main.tex")
		if replaceParent {
			name, target = filepath.Join(root, "sections"), outside
		}
		if err := os.RemoveAll(name); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, name); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := HashSelected(files, 1024); err == nil {
			t.Fatal("hash followed replaced symlink")
		}
		if err := CreateFiles(io.Discard, files); err == nil {
			t.Fatal("archive followed replaced symlink")
		}
		if _, err := ReadFile(files[0], 1024); err == nil {
			t.Fatal("scanner followed replaced symlink")
		}
	}
}

func TestReadSelectedFileEnforcesCurrentSize(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "main.tex")
	mustWrite(t, name, "large content")
	if _, err := ReadFile(File{Path: "main.tex", Source: name, Size: 1}, 4); err == nil {
		t.Fatal("read trusted stale size metadata")
	}
}
