package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDetectMissingFilesFromTeXDiagnostics(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "main.log")
	if err := os.WriteFile(
		logPath,
		[]byte("! Package pdftex.def Error: File `figures/plot.png' not found: using draft setting.\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	got := detectMissingFiles(
		[]byte("! LaTeX Error: File `sections/body.tex' not found.\n"),
		[]byte("! I can't find file `chapter2'.\n"),
		[]File{{RelativePath: "main.log", Workspace: filepath.Dir(logPath)}},
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

func TestDetectMissingFilesBoundsAndSortsRequests(t *testing.T) {
	var log strings.Builder
	for i := 99; i >= 0; i-- {
		fmt.Fprintf(&log, "! LaTeX Error: File `file-%03d.tex' not found.\n", i)
	}
	files := detectMissingFiles([]byte(log.String()), nil, nil)
	if len(files) != maxMissingFiles || !sort.StringsAreSorted(files) {
		t.Fatalf("requests=%v", files)
	}
	for _, file := range files {
		if cleanMissingPath(file) != file {
			t.Fatalf("unsafe request=%q", file)
		}
	}
}

func BenchmarkDetectRepeatedMissingFiles(b *testing.B) {
	log := []byte(strings.Repeat("! LaTeX Error: File `safe.tex' not found.\n", 100000))
	for b.Loop() {
		detectMissingFiles(log, nil, nil)
	}
}

func TestMissingPathsUsePortableProtocolRules(t *testing.T) {
	for _, name := range []string{"C:secret.tex", "folder/file:stream", "folder\\file.tex", "../escape", "/absolute", ".", "control\x00value"} {
		if clean := cleanMissingPath(name); clean != "" {
			t.Errorf("accepted %q as %q", name, clean)
		}
	}
	if clean := cleanMissingPath("./folder/main.tex"); clean != "folder/main.tex" {
		t.Fatal(clean)
	}
}
