package exporthistory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func publish(t *testing.T, root, group, name, content string, keep *int) error {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	return Publish(
		root,
		group,
		File{Path: name, Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:])},
		keep,
		func() error {
			fs, err := safefs.Open(root)
			if err != nil {
				return err
			}
			defer func() { _ = fs.Close() }()
			_, err = fs.WriteAtomic(
				name,
				int64(len(content)),
				func(w io.Writer) error { _, err := io.WriteString(w, content); return err },
			)
			return err
		},
	)
}

func TestCaseAliasesNeverDeleteTheNewestExport(t *testing.T) {
	root := t.TempDir()
	if err := publish(t, root, "paper", "Paper.pdf", "same", nil); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filepath.Join(root, "Paper.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	alias, err := os.Stat(filepath.Join(root, "paper.pdf"))
	if err != nil || !os.SameFile(original, alias) {
		t.Skip("filesystem is case-sensitive")
	}
	keep := 1
	if err := publish(t, root, "paper", "paper.pdf", "same", &keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "paper.pdf")); err != nil {
		t.Fatal("removed latest export through its old spelling", err)
	}
	paths, err := Paths(root)
	if err != nil || len(paths) != 1 {
		t.Fatal("case alias counted twice", paths, err)
	}
}

func TestOtherGroupsAreProtectedByFileIdentity(t *testing.T) {
	root := t.TempDir()
	if err := publish(t, root, "first", "old.pdf", "same", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "old.pdf"), filepath.Join(root, "alias.pdf")); err != nil {
		t.Skip(err)
	}
	digest := sha256.Sum256([]byte("same"))
	if err := Publish(
		root,
		"second",
		File{Path: "alias.pdf", Size: 4, SHA256: hex.EncodeToString(digest[:])},
		nil,
		func() error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	keep := 1
	if err := publish(t, root, "first", "latest.pdf", "same", &keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.pdf")); err != nil {
		t.Fatal("removed another group's physical file", err)
	}
}

func TestCleanupPreservesReplacedSymlinkAndDirectory(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := publish(t, root, "paper", "old.pdf", "old", nil); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "old.pdf")); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(
					filepath.Join(t.TempDir(), "outside.pdf"),
					filepath.Join(root, "old.pdf"),
				); err != nil {
					t.Skip(err)
				}
			} else if err := os.Mkdir(filepath.Join(root, "old.pdf"), 0700); err != nil {
				t.Fatal(err)
			}
			keep := 1
			if err := publish(t, root, "paper", "new.pdf", "new", &keep); err != nil {
				t.Fatal("user-replaced output blocked new publication", err)
			}
			if _, err := os.Lstat(filepath.Join(root, "old.pdf")); err != nil {
				t.Fatal("removed user's replacement", err)
			}
		})
	}
}

func TestUnwritableHistoryFailsBeforePublishingOutput(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permission enforcement")
	}
	root := t.TempDir()
	if err := publish(t, root, "paper", "old.pdf", "old", nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, directory)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	called := false
	digest := sha256.Sum256([]byte("new"))
	err := Publish(
		root,
		"paper",
		File{Path: "new.pdf", Size: 3, SHA256: hex.EncodeToString(digest[:])},
		nil,
		func() error { called = true; return nil },
	)
	if err == nil || called {
		t.Fatal("publication preceded writable history staging", called, err)
	}
}

func TestCleanupRequiresTheNewPublicationToRemainVerified(t *testing.T) {
	root := t.TempDir()
	if err := publish(t, root, "paper", "old.pdf", "old", nil); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("new"))
	keep := 1
	err := Publish(
		root,
		"paper",
		File{Path: "new.pdf", Size: 3, SHA256: hex.EncodeToString(digest[:])},
		&keep,
		func() error {
			// Model an editor replacing the newly published path before cleanup.
			return os.WriteFile(filepath.Join(root, "new.pdf"), []byte("user edit"), 0600)
		},
	)
	if err == nil {
		t.Fatal("cleanup accepted a changed latest export")
	}
	if _, err := os.Stat(filepath.Join(root, "old.pdf")); err != nil {
		t.Fatal("removed last verified export", err)
	}
}

func TestRetentionPersistsAcrossCallsAndDeduplicatesPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.pdf", "b.pdf", "a.pdf", "c.pdf"} {
		if err := publish(t, root, "paper", name, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := Paths(root)
	if err != nil || len(paths) != 3 {
		t.Fatalf("paths=%v %v", paths, err)
	}
	keep := 2
	if err := publish(t, root, "paper", "d.pdf", "d", &keep); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.pdf", "b.pdf"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old export retained: %s %v", name, err)
		}
	}
	for _, name := range []string{"c.pdf", "d.pdf"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetentionPreservesEditedAndUnmanagedFilesAndFailedPublication(t *testing.T) {
	root := t.TempDir()
	if err := publish(t, root, "paper", "old.pdf", "original", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old.pdf", "unmanaged.pdf"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("user content"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	keep := 1
	err := Publish(root, "paper", File{Path: "new.pdf", Size: 1, SHA256: hex.EncodeToString(make([]byte, 32))}, &keep,
		func() error { return errors.New("failed build/export") })
	if err == nil {
		t.Fatal("accepted failed export")
	}
	paths, err := Paths(root)
	if err != nil || len(paths) != 1 {
		t.Fatalf("failed write changed history: %v %v", paths, err)
	}
	if err := publish(t, root, "paper", "new.pdf", "new", &keep); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old.pdf", "unmanaged.pdf"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != "user content" {
			t.Fatalf("deleted user file %s: %q %v", name, data, err)
		}
	}
}

func TestHistoryRejectsEscapesCorruptionAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := publish(t, root, "paper", "../escape.pdf", "x", nil); err == nil {
		t.Fatal("accepted escaping export")
	}
	if err := publish(t, root, "paper", "main.tex", "x", nil); err == nil {
		t.Fatal("accepted source path")
	}
	if err := publish(t, root, "paper", "old.pdf", "old", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, manifestPath("paper")),
		[]byte(`{"version":1,"files":[{"path":"../escape.pdf"}]}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := Publish(root, "paper", File{Path: "new.pdf", Size: 0, SHA256: hex.EncodeToString(make([]byte, 32))}, nil,
		func() error { called = true; return nil }); err == nil || called {
		t.Fatal("corrupt history reached publication")
	}
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, ".latexmk-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(other, directory)); err != nil {
		t.Skip(err)
	}
	if _, err := Paths(other); err == nil {
		t.Fatal("followed symlinked history")
	}
}

func TestHistoryPreviewDoesNotCreateState(t *testing.T) {
	root := t.TempDir()
	paths, err := Paths(root)
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".latexmk-cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reader created state")
	}
}

func TestRetentionPreservesPathsOwnedByAnotherGroup(t *testing.T) {
	root := t.TempDir()
	if err := publish(t, root, "first", "shared.pdf", "same content", nil); err != nil {
		t.Fatal(err)
	}
	if err := publish(t, root, "second", "shared.pdf", "same content", nil); err != nil {
		t.Fatal(err)
	}
	keep := 1
	if err := publish(t, root, "first", "new.pdf", "new", &keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "shared.pdf")); err != nil {
		t.Fatal("removed another group's export", err)
	}
}
