package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFreezeKeepsHashedBytesAfterEditorReplacement(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.tex")
	if err := os.WriteFile(source, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	frozen, err := Freeze(context.Background(), []File{{Path: "main.tex", Source: source}}, 1, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = frozen.Close() }()
	if err := os.WriteFile(source, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(frozen.Files[0], 5)
	if err != nil || string(data) != "first" || frozen.Files[0].SHA256 == "" {
		t.Fatalf("snapshot=%q %v", data, err)
	}
	if _, err := Freeze(context.Background(), []File{{Path: "main.tex", Source: source}}, 1, 4, nil); err == nil {
		t.Fatal("byte quota was ignored")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "secret"), source); err != nil {
		t.Fatal(err)
	}
	if _, err := Freeze(context.Background(), []File{{Path: "main.tex", Source: source}}, 1, 5, nil); err == nil {
		t.Fatal("symlink was captured")
	}
}

func TestFreezeReusesContentButDetectsMetadataPreservingEdits(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "figure.bin")
	data := bytes.Repeat([]byte("a"), 1<<20)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: "figure.bin", Source: source}}
	first, err := Freeze(context.Background(), files, 1, int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	reused, err := Freeze(context.Background(), files, 1, int64(len(data)), first)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reused.Close() }()
	before, _ := os.Stat(first.Files[0].Source)
	after, _ := os.Stat(reused.Files[0].Source)
	if reused.ReusedBytes != int64(len(data)) || !os.SameFile(before, after) {
		t.Fatal("unchanged large capture was copied instead of linked")
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'b'
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err := Freeze(context.Background(), files, 1, int64(len(data)), reused)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = changed.Close() }()
	if changed.ReusedBytes != 0 || changed.Files[0].SHA256 == reused.Files[0].SHA256 {
		t.Fatal("same size/mtime bypassed current content verification")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if captured, err := ReadFile(reused.Files[0], int64(len(data))); err != nil || captured[0] != 'a' {
		t.Fatal("closing the previous spool invalidated the linked generation")
	}
	if _, err := Freeze(context.Background(), files, 1, int64(len(data)-1), changed); err == nil {
		t.Fatal("content reuse bypassed the byte limit")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(changed.Files[0].Source, source); err != nil {
		t.Fatal(err)
	}
	if _, err := Freeze(context.Background(), files, 1, int64(len(data)), changed); err == nil {
		t.Fatal("reused an unapproved symlink source")
	}
}

func TestMissingCaptureFallsBackToVerifiedCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "figure.bin")
	data := bytes.Repeat([]byte("data"), 1<<18)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: "figure.bin", Source: source}}
	first, err := Freeze(context.Background(), files, 1, int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Freeze(context.Background(), files, 1, int64(len(data)), first)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	if next.ReusedBytes != 0 || next.Files[0].SHA256 != first.Files[0].SHA256 {
		t.Fatal("missing cached file changed capture correctness")
	}
}

func BenchmarkSnapshotCapture(b *testing.B) {
	source := filepath.Join(b.TempDir(), "figure.bin")
	data := bytes.Repeat([]byte("image bytes"), (16<<20)/11)
	if err := os.WriteFile(source, data, 0600); err != nil {
		b.Fatal(err)
	}
	files := []File{{Path: "figure.bin", Source: source}}
	first, err := Freeze(context.Background(), files, 1, int64(len(data)), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	for _, tc := range []struct {
		name     string
		previous *Frozen
	}{{"copy", nil}, {"verified-reuse", first}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				next, err := Freeze(context.Background(), files, 1, int64(len(data)), tc.previous)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(next.ReusedBytes), "reused-bytes/op")
				if err := next.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
