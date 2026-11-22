package resources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	shared "github.com/billstark001/latexmk/packages/shared/resources"
)

func writeResource(t *testing.T, root, name, value string) {
	t.Helper()
	source := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureFreezesAndRemountsWithoutPublishingOriginalPaths(t *testing.T) {
	root := t.TempDir()
	writeResource(t, root, "private/fonts/font.otf", "original font")
	writeResource(t, root, "private/fonts/OFL.txt", "license")
	writeResource(t, root, "unused/confidential.dat", "private")
	specs := map[string]shared.Spec{
		"font": {
			Type:           "files",
			Kind:           "font",
			Path:           "private/fonts",
			Mount:          "fonts/paper",
			Include:        []string{"*.otf"},
			LicenseFiles:   []string{"OFL.txt"},
			Redistribution: "allow",
		},
		"unused": {Type: "files", Path: "unused", Mount: "data"},
	}
	captured, err := Capture(context.Background(), root, specs, []string{"font"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	if len(captured.DenyPaths) != 2 {
		t.Fatalf("unused resource source was not reserved: %#v", captured.DenyPaths)
	}
	writeResource(t, root, "private/fonts/font.otf", "edited font")
	mounted := captured.MountFilesByID["font"]
	if len(mounted) != 2 {
		t.Fatalf("license missing: %#v", mounted)
	}
	for _, file := range mounted {
		if file.Path == "fonts/paper/font.otf" {
			data, err := projectarchive.ReadFile(file, 100)
			if err != nil || string(data) != "original font" {
				t.Fatalf("mounted bytes not immutable: %q %v", data, err)
			}
		}
	}
	for _, file := range captured.Resources[0].Files {
		if file.Path != "font.otf" && file.Path != "OFL.txt" {
			t.Fatalf("source path leaked into manifest: %q", file.Path)
		}
	}
}

func TestCaptureRejectsCredentialAndSymlinkResources(t *testing.T) {
	root := t.TempDir()
	writeResource(t, root, "private/id_rsa", "secret")
	if captured, err := Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"secret": {Type: "files", Path: "private/id_rsa"}},
		[]string{"secret"},
	); err == nil {
		_ = captured.Close()
		t.Fatal("credential was captured")
	}
	writeResource(t, root, "private/custom-token.txt", "private token")
	if captured, err := Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"secret": {Type: "files", Path: "private/custom-token.txt"}},
		[]string{"secret"},
		filepath.Join(root, "private/custom-token.txt"),
	); err == nil {
		_ = captured.Close()
		t.Fatal("custom credential source was captured")
	}
	outside := t.TempDir()
	writeResource(t, outside, "font.otf", "outside")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skip(err)
	}
	if captured, err := Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"font": {Type: "files", Path: "linked"}},
		[]string{"font"},
	); err == nil {
		_ = captured.Close()
		t.Fatal("symlink was followed")
	}
}

func TestCaptureOwnsFrozenBytesAndClosesAllLogicalViews(t *testing.T) {
	root := t.TempDir()
	writeResource(t, root, "private/Body.otf", "font bytes")
	captured, err := Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"font": {Type: "files", Path: "private", Mount: "fonts"}},
		[]string{"font"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	mounted := captured.MountFilesByID["font"][0]
	blob := captured.Blobs[mounted.SHA256]
	sourceInfo, err := os.Stat(filepath.Join(root, "private/Body.otf"))
	if err != nil {
		t.Fatal(err)
	}
	blobInfo, err := os.Stat(blob.Source)
	if err != nil {
		t.Fatal(err)
	}
	mountedInfo, err := os.Stat(mounted.Source)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, blobInfo) {
		t.Fatal("immutable capture still aliases the editable source")
	}
	if !os.SameFile(blobInfo, mountedInfo) {
		t.Fatal("logical mount copied bytes instead of sharing the owned capture")
	}
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []projectarchive.File{blob, mounted} {
		if _, err := os.Stat(file.Source); !os.IsNotExist(err) {
			t.Fatalf("Close retained captured file %s: %v", file.Path, err)
		}
	}
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureIgnoresUnselectedCredentialCandidates(t *testing.T) {
	root := t.TempDir()
	writeResource(t, root, "private/Body.otf", "font")
	writeResource(t, root, "private/token.txt", "secret")
	captured, err := Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"font": {Type: "files", Path: "private", Mount: "fonts", Include: []string{"*.otf"}}},
		[]string{"font"},
		filepath.Join(root, "private/token.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	if len(captured.Resources[0].Files) != 1 || captured.Resources[0].Files[0].Path != "Body.otf" {
		t.Fatalf("unexpected selection: %#v", captured.Resources[0].Files)
	}
}

func TestCaptureRejectsCanceledRequestsBeforeAllocatingResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if captured, err := Capture(
		ctx,
		t.TempDir(),
		map[string]shared.Spec{"package": {Type: "texlive", Packages: []string{"algorithms"}}},
		[]string{"package"},
	); err == nil {
		_ = captured.Close()
		t.Fatal("canceled request produced a capture")
	}
}
