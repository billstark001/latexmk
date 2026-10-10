package client

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type firstReadHook struct {
	io.Reader
	hook func()
}

func (r *firstReadHook) Read(p []byte) (int, error) {
	if r.hook != nil {
		hook := r.hook
		r.hook = nil
		hook()
	}
	return r.Reader.Read(p)
}

func TestArtifactPublicationStaysInsideOpenedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacing an open directory requires Unix rename semantics")
	}
	parent, outside := t.TempDir(), t.TempDir()
	root, moved := filepath.Join(parent, "project"), filepath.Join(parent, "original")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	data := "verified PDF"
	digest := sha256.Sum256([]byte(data))
	reader := &firstReadHook{Reader: strings.NewReader(data), hook: func() {
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		// Populate the replacement directory with a staged-name lookalike. A
		// path-based rename must not publish this unverified file after hashing.
		entries, err := os.ReadDir(moved)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if err := os.WriteFile(filepath.Join(outside, entry.Name()), []byte("unverified"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}}
	if err := writeArtifact(root, "main.pdf", reader, int64(len(data)), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "main.pdf")); err != nil || string(got) != data {
		t.Fatalf("original root did not receive verified output: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "main.pdf")); !os.IsNotExist(err) {
		t.Fatalf("publication escaped into replacement root: %v", err)
	}
}

func TestArtifactFailurePreservesExistingOutput(t *testing.T) {
	data := "new PDF"
	digest := sha256.Sum256([]byte(data))
	for _, tc := range []struct{ name, payload, hash string }{
		{"truncated", data[:3], hex.EncodeToString(digest[:])},
		{"extra bytes", data + "hidden", hex.EncodeToString(digest[:])},
		{"wrong hash", data, strings.Repeat("0", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "main.pdf"), []byte("old PDF"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := writeArtifact(
				root,
				"main.pdf",
				strings.NewReader(tc.payload),
				int64(len(data)),
				tc.hash,
			); err == nil {
				t.Fatal("accepted invalid artifact")
			}
			if got, err := os.ReadFile(filepath.Join(root, "main.pdf")); err != nil || string(got) != "old PDF" {
				t.Fatalf("lost old output: %q err=%v", got, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("left incomplete stage: %v err=%v", entries, err)
			}
		})
	}
}
