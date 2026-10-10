package pack

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

func capture(t *testing.T, content map[string]string) *projectarchive.Frozen {
	t.Helper()
	root := t.TempDir()
	var files []projectarchive.File
	for name, data := range content {
		source := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		files = append(files, projectarchive.File{Path: name, Source: source})
	}
	frozen, err := projectarchive.Freeze(context.Background(), files, MaxFiles, MaxBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := frozen.Close(); err != nil {
			t.Error(err)
		}
	})
	return frozen
}

func TestZIPIsDeterministicAndUsesCapturedBytes(t *testing.T) {
	frozen := capture(t, map[string]string{"main.tex": "source", "figures/a.pdf": "graphic"})
	var first, second bytes.Buffer
	if err := writeZIP(context.Background(), &first, frozen.Files); err != nil {
		t.Fatal(err)
	}
	frozen.Files[0], frozen.Files[1] = frozen.Files[1], frozen.Files[0]
	if err := writeZIP(context.Background(), &second, frozen.Files); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("ZIP depends on input order or clock")
	}
	archive, err := zip.NewReader(bytes.NewReader(first.Bytes()), int64(first.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 2 || archive.File[0].Name != "figures/a.pdf" || archive.File[1].Name != "main.tex" {
		t.Fatalf("archive members: %+v", archive.File)
	}
	for _, file := range archive.File {
		if !file.Mode().IsRegular() || file.Mode().Perm() != 0644 {
			t.Fatalf("unexpected member mode: %v", file.Mode())
		}
	}
}

func TestFailedZIPPreservesExistingOutput(t *testing.T) {
	frozen := capture(t, map[string]string{"main.tex": "original"})
	if err := os.WriteFile(frozen.Files[0].Source, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "paper.zip")
	if err := os.WriteFile(output, []byte("previous valid package"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), output, frozen.Files); err == nil {
		t.Fatal("accepted changed captured member")
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "previous valid package" {
		t.Fatalf("failed export replaced output: %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil || len(entries) != 1 {
		t.Fatalf("abandoned staged ZIP: %+v, %v", entries, err)
	}
}

func TestZIPRejectsUnsafeDuplicateAndSymlinkMembers(t *testing.T) {
	frozen := capture(t, map[string]string{"main.tex": "source"})
	for _, name := range []string{"../escape.tex", "a/../main.tex", "/absolute.tex", `a\b.tex`} {
		file := frozen.Files[0]
		file.Path = name
		if err := writeZIP(context.Background(), &bytes.Buffer{}, []projectarchive.File{file}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := writeZIP(context.Background(), &bytes.Buffer{}, append(frozen.Files, frozen.Files...)); err == nil {
		t.Fatal("accepted duplicate")
	}
	outside := filepath.Join(t.TempDir(), "outside.tex")
	if err := os.WriteFile(outside, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(frozen.Files[0].Source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, frozen.Files[0].Source); err != nil {
		t.Fatal(err)
	}
	if err := writeZIP(context.Background(), &bytes.Buffer{}, frozen.Files); err == nil {
		t.Fatal("followed a replaced symlink")
	}
}

func TestArxivSourcesKeepFiguresAndSubmissionOutputs(t *testing.T) {
	files := []projectarchive.File{{Path: "main.tex"}, {Path: "main.pdf"}, {Path: "main.aux"},
		{Path: "figures/plot.pdf"}, {Path: "main.bbl"}, {Path: "main.ind"}, {Path: "main.gls"}, {Path: "main.nls"}}
	selected, err := Sources(files, "main.tex", "arxiv")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range selected {
		paths = append(paths, file.Path)
	}
	if strings.Join(paths, ",") != "main.tex,figures/plot.pdf,main.bbl,main.ind,main.gls,main.nls" {
		t.Fatal(paths)
	}
	if _, err := Sources([]projectarchive.File{{Path: ".hidden/input.tex"}}, "main.tex", "arxiv"); err == nil {
		t.Fatal("accepted hidden dependency")
	}
}

func TestGeneratedOutputCannotReplaceSource(t *testing.T) {
	source := []projectarchive.File{{Path: "main.bbl", Size: 3, SHA256: "source"}}
	generated := []projectarchive.File{{Path: "main.bbl", Size: 3, SHA256: "generated"}}
	if _, err := Merge(source, generated); err == nil {
		t.Fatal("generated output replaced source")
	}
}

func TestCancelledEmptyPackPreservesExistingOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := filepath.Join(t.TempDir(), "paper.zip")
	original := []byte("previous package")
	if err := os.WriteFile(output, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Write(ctx, output, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pack error=%v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("output=%q, error=%v", data, err)
	}
}
