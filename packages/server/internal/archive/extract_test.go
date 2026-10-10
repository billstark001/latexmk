package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	_, err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{MaxFiles: 10, MaxBytes: 10})
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestExtractsRegularFile(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "dir/main.tex", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("abc"))
	_ = tw.Close()
	_ = gz.Close()
	root := t.TempDir()
	stats, err := ExtractTarGz(bytes.NewReader(buf.Bytes()), root, Limits{MaxFiles: 10, MaxBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 1 || stats.Bytes != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	b, err := os.ReadFile(filepath.Join(root, "dir", "main.tex"))
	if err != nil || string(b) != "abc" {
		t.Fatalf("content=%q err=%v", b, err)
	}
}

func TestExtractVerifiesGzipTrailer(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	valid := buf.Bytes()
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 1
	for name, data := range map[string][]byte{
		"checksum":  corrupt,
		"truncated": valid[:len(valid)-8],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractTarGz(bytes.NewReader(data), t.TempDir(), Limits{}); err == nil {
				t.Fatal("accepted an invalid gzip trailer")
			}
		})
	}
}

func TestExtractRejectsNegativeLimits(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	for _, limits := range []Limits{{MaxFiles: -1}, {MaxBytes: -1}} {
		if _, err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), limits); err == nil {
			t.Fatalf("accepted limits %+v", limits)
		}
	}
}
