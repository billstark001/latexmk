package watch

import (
	"context"
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
	tracker, err := New([]Target{{Name: ".gitignore", Path: missing}, {Name: "chapter.tex", Path: present}}, 5*time.Millisecond, 10*time.Millisecond)
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
