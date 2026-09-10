package safefs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testRoot(t *testing.T) *Root {
	t.Helper()
	root, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func textWriter(text string) func(io.Writer) error {
	return func(w io.Writer) error { _, err := io.WriteString(w, text); return err }
}

func TestCleanProtocolPaths(t *testing.T) {
	for _, name := range []string{"", ".", "../secret", "/absolute", "x/../../secret", "a\\b", "a\x00b", "C:/secret", strings.Repeat("x", 4097)} {
		if _, err := Clean(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if got, err := Clean("./chapters/../main.tex"); err != nil || got != "main.tex" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestLimitedIOAndExclusiveRollback(t *testing.T) {
	root := testRoot(t)
	if err := root.WriteExclusive("nested/data", 3, textWriter("abc")); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteExclusive("nested/data", 3, textWriter("new")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := root.ReadLimited("nested/data", 2); !errors.Is(err, ErrLimit) {
		t.Fatalf("read limit: %v", err)
	}
	if err := root.WriteExclusive("partial", 2, textWriter("abc")); !errors.Is(err, ErrLimit) {
		t.Fatalf("write limit: %v", err)
	}
	if _, err := root.Lstat("partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial remains: %v", err)
	}
	if data, err := root.ReadLimited("nested/data", 3); err != nil || string(data) != "abc" {
		t.Fatalf("old file: %q %v", data, err)
	}
	if err := root.MakeDirs("directory", 0700); err != nil {
		t.Fatal(err)
	}
	if f, err := root.OpenRegular("directory"); err == nil {
		_ = f.Close()
		t.Fatal("opened directory")
	}
}

func TestAtomicPublicationPreservesPreviousOnFailure(t *testing.T) {
	root := testRoot(t)
	if _, err := root.WriteAtomic("result", 10, textWriter("old")); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("encoder failed")
	for _, tc := range []struct {
		max   int64
		write func(io.Writer) error
		want  error
	}{
		{2, textWriter("large"), ErrLimit},
		{10, func(w io.Writer) error { _, err := io.WriteString(w, "partial"); return errors.Join(injected, err) }, injected},
	} {
		if _, err := root.WriteAtomic("result", tc.max, tc.write); !errors.Is(err, tc.want) {
			t.Fatalf("%v", err)
		}
		if data, err := root.ReadLimited("result", 10); err != nil || string(data) != "old" {
			t.Fatalf("lost old result: %q %v", data, err)
		}
	}
	pending, err := root.Stage("result", 10, textWriter("new"))
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := root.ReadLimited("result", 10); string(data) != "old" {
		t.Fatal("published before commit")
	}
	if err := pending.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	if data, _ := root.ReadLimited("result", 10); string(data) != "new" {
		t.Fatal("not published")
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
}

func TestAtomicReadersNeverObservePartialGeneration(t *testing.T) {
	root := testRoot(t)
	a, b := strings.Repeat("a", 8192), strings.Repeat("b", 8192)
	if _, err := root.WriteAtomic("result", 8192, textWriter(a)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			data, err := root.ReadLimited("result", 8192)
			if err != nil {
				failures <- err
				return
			}
			if string(data) != a && string(data) != b {
				failures <- errors.New("partial generation observed")
				return
			}
		}
	}()
	for range 20 {
		if _, err := root.WriteAtomic("result", 8192, textWriter(b)); err != nil {
			t.Error(err)
			break
		}
		if _, err := root.WriteAtomic("result", 8192, textWriter(a)); err != nil {
			t.Error(err)
			break
		}
	}
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func TestSymlinksCannotReadWriteOrPublishOutsideRoot(t *testing.T) {
	root := testRoot(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink(outside, "escape"); err != nil {
		t.Skip(err)
	}
	if _, err := root.ReadLimited("escape/secret", 100); err == nil {
		t.Fatal("read escaped")
	}
	if err := root.WriteExclusive("escape/new", 100, textWriter("bad")); err == nil {
		t.Fatal("write escaped")
	}
	if _, err := root.WriteAtomic("escape/secret", 100, textWriter("bad")); err == nil {
		t.Fatal("publication escaped")
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "secret" {
		t.Fatal("outside content changed")
	}
	if err := root.WriteExclusive("inside", 10, textWriter("inside")); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("inside", "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReadLimited("alias", 100); err == nil {
		t.Fatal("internal symlink violates policy")
	}
}

func TestVerifiedCopyRejectsChangedAndExtraContent(t *testing.T) {
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, data := range []string{"ab", "abcd", "xyz"} {
		if err := CopyVerified(io.Discard, strings.NewReader(data), 3, digest); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	var out bytes.Buffer
	if err := CopyVerified(&out, strings.NewReader("abc"), 3, digest); err != nil || out.String() != "abc" {
		t.Fatal(err)
	}
}

func TestPublicationCannotSuppressWriteFailure(t *testing.T) {
	root := testRoot(t)
	_, err := root.WriteAtomic("result", 2, func(w io.Writer) error {
		_, _ = io.WriteString(w, "too large")
		return nil
	})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("suppressed write failure: %v", err)
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed publication leaked files: %v %v", entries, err)
	}
}

func TestEncoderPanicCleansTemporaryFiles(t *testing.T) {
	root := testRoot(t)
	for _, atomic := range []bool{false, true} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("encoder panic was swallowed")
				}
			}()
			write := func(w io.Writer) error {
				_, _ = io.WriteString(w, "partial")
				panic("encoder failed")
			}
			if atomic {
				_, _ = root.WriteAtomic("result", 10, write)
			} else {
				_ = root.WriteExclusive("result", 10, write)
			}
		}()
		entries, err := os.ReadDir(root.Name())
		if err != nil || len(entries) != 0 {
			t.Fatalf("encoder panic leaked files: %v %v", entries, err)
		}
	}
}
