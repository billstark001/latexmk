package resources

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	shared "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestCaptureKeepsEachResourcePermissionsWhenIdenticalBytesShareMount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX permission metadata")
	}
	root := t.TempDir()
	specs := make(map[string]shared.Spec)
	for name, mode := range map[string]os.FileMode{"first": 0750, "second": 0640} {
		writeResource(t, root, name+"/run.sh", "identical content")
		if err := os.Chmod(filepath.Join(root, name, "run.sh"), mode); err != nil {
			t.Fatal(err)
		}
		specs[name] = shared.Spec{Type: "files", Kind: "data", Path: name, Mount: "tools", Redistribution: "allow"}
	}
	captured, err := Capture(context.Background(), root, specs, []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	for name, mode := range map[string]os.FileMode{"first": 0750, "second": 0640} {
		file := captured.MountFilesByID[name][0]
		if file.SourcePermissions == nil || *file.SourcePermissions != mode {
			t.Fatalf("resource %s inherited another source's mode: %+v", name, file)
		}
		info, err := os.Stat(file.Source)
		if err != nil || info.Mode() != 0600 {
			t.Fatalf("source mode escaped into private spool: %+v %v", info, err)
		}
	}
}
