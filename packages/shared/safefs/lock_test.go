package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockContendsAndReleasesWithoutRemovingInode(t *testing.T) {
	root := testRoot(t)
	first, err := root.TryLock("locks/publisher")
	if err != nil {
		t.Fatal(err)
	}
	if second, err := root.TryLock("locks/publisher"); !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second publisher acquired held lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("locks/publisher"); err != nil {
		t.Fatal("lock inode was removed", err)
	}
	third, err := root.TryLock("locks/publisher")
	if err != nil {
		t.Fatal("lock was not released", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTryLockRejectsEscapesSymlinksAndDirectories(t *testing.T) {
	root := testRoot(t)
	if err := root.MakeDirs("directory", 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "directory"} {
		if lock, err := root.TryLock(name); err == nil {
			_ = lock.Close()
			t.Fatalf("accepted %q", name)
		}
	}
	outside := t.TempDir()
	if err := root.Symlink(outside, "linked"); err != nil {
		t.Skip(err)
	}
	if lock, err := root.TryLock("linked/publisher"); err == nil {
		_ = lock.Close()
		t.Fatal("followed symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "publisher")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created outside root", err)
	}
}
