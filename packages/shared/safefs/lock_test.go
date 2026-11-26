package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestTryLockConcurrentInitialCreationPreservesOneCoordinatingInode(t *testing.T) {
	root := testRoot(t)
	if err := root.MakeDirs("locks", 0700); err != nil {
		t.Fatal(err)
	}
	type result struct {
		lock *FileLock
		err  error
	}
	for round := range 64 {
		name := fmt.Sprintf("locks/object-%d.lock", round)
		start := make(chan struct{})
		results := make(chan result, 8)
		var workers sync.WaitGroup
		for range cap(results) {
			workers.Go(func() {
				<-start
				lock, err := root.TryLock(name)
				results <- result{lock, err}
			})
		}
		close(start)
		workers.Wait()
		close(results)
		var held []*FileLock
		for attempt := range results {
			if attempt.err != nil && !errors.Is(attempt.err, ErrLocked) {
				t.Errorf("concurrent initial lock creation failed: %v", attempt.err)
			}
			if attempt.lock != nil {
				held = append(held, attempt.lock)
			}
		}
		// Winners stay open until every contender has tried, making simultaneous
		// creation and mutual exclusion deterministic without timing sleeps.
		if len(held) != 1 {
			for _, lock := range held {
				_ = lock.Close()
			}
			t.Fatalf("simultaneous publishers held %d locks, want one", len(held))
		}
		first, err := held[0].file.Stat()
		if err != nil {
			_ = held[0].Close()
			t.Fatal(err)
		}
		if err := held[0].Close(); err != nil {
			t.Fatal(err)
		}
		next, err := root.TryLock(name)
		if err != nil {
			t.Fatal(err)
		}
		after, err := next.file.Stat()
		closeErr := next.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("reopen coordinating inode: %v %v", err, closeErr)
		}
		if !os.SameFile(first, after) {
			t.Fatal("contention replaced the coordinating lock inode")
		}
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
