package compile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectMissingFilesFromTeXDiagnostics(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "main.log")
	if err := os.WriteFile(logPath, []byte("! Package pdftex.def Error: File `figures/plot.png' not found: using draft setting.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := detectMissingFiles(
		[]byte("! LaTeX Error: File `sections/body.tex' not found.\n"),
		[]byte("! I can't find file `chapter2'.\n"),
		[]File{{RelativePath: "main.log", AbsolutePath: logPath}},
	)
	want := []string{"chapter2", "figures/plot.png", "sections/body.tex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing files = %#v, want %#v", got, want)
	}
}

func TestDetectMissingFilesRejectsUnsafePathsAndDeduplicates(t *testing.T) {
	input := []byte("! LaTeX Error: File `../secret.tex' not found.\n" +
		"! LaTeX Error: File `/etc/passwd' not found.\n" +
		"! LaTeX Error: File `safe.tex' not found.\n" +
		"! I can't find file `safe.tex'.\n")
	got := detectMissingFiles(input, nil, nil)
	want := []string{"safe.tex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing files = %#v, want %#v", got, want)
	}
}
