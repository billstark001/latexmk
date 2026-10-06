package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTrackerDebouncesRapidChanges(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.tex")
	if err := os.WriteFile(file, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker, err := New([]Target{{Name: "main.tex", Path: file}}, 5*time.Millisecond, 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan []string, 1)
	errs := make(chan error, 1)
	go func() {
		changed, err := tracker.Wait(ctx)
		if err != nil {
			errs <- err
			return
		}
		result <- changed
	}()
	if err := os.WriteFile(file, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(file, []byte("three"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case changed := <-result:
		if !reflect.DeepEqual(changed, []string{"main.tex"}) {
			t.Fatalf("changed = %#v", changed)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestTrackerDetectsCreationAndDeletion(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, ".gitignore")
	present := filepath.Join(root, "chapter.tex")
	if err := os.WriteFile(present, []byte("chapter"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracker, err := New(
		[]Target{{Name: ".gitignore", Path: missing}, {Name: "chapter.tex", Path: present}},
		5*time.Millisecond,
		10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missing, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(present); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	changed, err := tracker.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "chapter.tex"}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %#v, want %#v", changed, want)
	}
}

func TestRefreshDetectsNewMatchingFile(t *testing.T) {
	root := t.TempDir()
	tracker, err := New(nil, 10*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	tracker.Refresh = func() ([]Target, error) {
		return []Target{{Name: "new.tex", Path: filepath.Join(root, "new.tex")}}, nil
	}
	if err := os.WriteFile(filepath.Join(root, "new.tex"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	changed, err := tracker.Wait(ctx)
	if err != nil || len(changed) != 1 || changed[0] != "new.tex" {
		t.Fatalf("new member: %v %v", changed, err)
	}
}

func TestNativeWatchDetectsAtomicReplacementWithUnchangedMetadata(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.tex")
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	tracker, err := New([]Target{{Name: "main.tex", Path: file}}, time.Hour, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := tracker.Wait(ctx); done <- err }()
	// Let Wait register its directory watch; hourly polling cannot satisfy this test.
	time.Sleep(50 * time.Millisecond)
	replacement := filepath.Join(root, "save.tmp")
	if err := os.WriteFile(replacement, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, file); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A second replacement must also be observed, despite the original inode disappearing.
	go func() { _, err := tracker.Wait(ctx); done <- err }()
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(replacement, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, file); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMaximumWaitBoundsContinuousEdits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "main.tex")
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	tracker, err := New([]Target{{Name: "main.tex", Path: file}}, 5*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tracker.MaxWait = 70 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = os.WriteFile(file, []byte(time.Now().String()), 0600)
			}
		}
	}()
	if _, err := tracker.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshFailureStopsInsteadOfUsingStalePolicy(t *testing.T) {
	tracker, err := New(nil, time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	tracker.Refresh = func() ([]Target, error) { return nil, os.ErrPermission }
	if _, err := tracker.Wait(context.Background()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v", err)
	}
}
