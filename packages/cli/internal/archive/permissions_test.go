package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFreezeCapturesOriginalPermissionsWithoutChangingPrivateSpoolOrUploads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX permission metadata")
	}
	source := filepath.Join(t.TempDir(), "figure.bin")
	contents := bytes.Repeat([]byte("x"), minReusableCaptureBytes+1)
	if err := os.WriteFile(source, contents, 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0751|os.ModeSetuid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	files := []File{{Path: "figure.bin", Source: source}}
	first, err := Freeze(context.Background(), files, 1, int64(len(contents)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if got := first.Files[0].SourcePermissions; got == nil || *got != 0751 {
		t.Fatalf("original rwx permissions were not captured safely: %v", got)
	}
	spool, err := os.Stat(first.Files[0].Source)
	if err != nil || spool.Mode() != 0600 {
		t.Fatalf("private spool acquired source permissions: %v %v", spool, err)
	}
	if err := os.Chmod(source, 0640); err != nil {
		t.Fatal(err)
	}
	second, err := Freeze(context.Background(), first.Files, 1, int64(len(contents)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if got := second.Files[0].SourcePermissions; got == nil || *got != 0751 {
		t.Fatalf("recapture used private spool or later source mode: %v", got)
	}
	reused, err := Freeze(context.Background(), files, 1, int64(len(contents)), first)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reused.Close() }()
	if reused.ReusedBytes != int64(len(contents)) || reused.Files[0].SourcePermissions == nil ||
		*reused.Files[0].SourcePermissions != 0640 {
		t.Fatalf("content reuse lost current source permissions: %+v", reused)
	}
	var upload bytes.Buffer
	if err := CreateFiles(&upload, first.Files); err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(&upload)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compressed.Close() }()
	header, err := tar.NewReader(compressed).Next()
	if err != nil || header.Mode != 0644 || header.Typeflag != tar.TypeReg {
		t.Fatalf("permission metadata changed compilation upload policy: %+v %v", header, err)
	}
}

func TestFreezeOwnsInheritedPermissionMetadata(t *testing.T) {
	source := filepath.Join(t.TempDir(), "main.tex")
	if err := os.WriteFile(source, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0750)
	first, err := Freeze(
		context.Background(),
		[]File{{Path: "main.tex", Source: source, SourcePermissions: &mode}},
		1,
		6,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	mode = 0600
	if *first.Files[0].SourcePermissions != 0750 {
		t.Fatal("frozen permissions alias the input metadata")
	}
	second, err := Freeze(context.Background(), first.Files, 1, 6, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	*first.Files[0].SourcePermissions = 0644
	if *second.Files[0].SourcePermissions != 0750 {
		t.Fatal("recaptured permissions alias the previous capture")
	}
}
