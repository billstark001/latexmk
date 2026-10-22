package archive

import (
	"path/filepath"
	"testing"
)

func TestWindowsHardDenyIgnoresPathCase(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"main.tex", "Secrets/AccessToken", ".ENV.LATEXMK", ".LATEXMK-TOKEN", "private/POLICY"} {
		mustWrite(t, filepath.Join(root, name), "# fixture\n")
	}
	opts := Options{
		Root:        root,
		DenyFiles:   []string{filepath.Join(root, "secrets", "accesstoken")},
		IgnoreFiles: []string{"private/policy"},
	}
	files, _, err := Manifest(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatalf("credential or policy file leaked into upload: %#v", files)
	}
}
