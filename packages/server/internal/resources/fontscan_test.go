package resources

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestFontScanHelperUsesRunnerNamespaceForOrphanCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Docker command uses a POSIX script")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "docker-args")
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then\n  printf '%s\\n' \"$@\" > \"$FONT_SCAN_CAPTURE\"\n  printf 'Paper\\tRegular\\n'\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FONT_SCAN_CAPTURE", capture)
	m := testManager(t)
	m.cfg.RunnerImage = "registry.example/runner@sha256:" + strings.Repeat("a", 64)
	m.cfg.RunnerNamespace = "font-scan-test"
	file := reviewFile("Paper.ttf", "font fixture")
	source := filepath.Join(dir, "font-source")
	if err := os.WriteFile(source, []byte("font fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	faces, err := m.scanFont(context.Background(), model.Baseline{Image: m.cfg.RunnerImage}, source, file)
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 1 || faces[0].Family != "Paper" {
		t.Fatalf("helper result not parsed: %+v", faces)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := "\n" + string(raw)
	for _, required := range []string{"\n--label\nlatexmk.runner=true\n", "\n--label\nlatexmk.runner.namespace=font-scan-test\n", "\n--pull=never\n", "\n--network=none\n"} {
		if !strings.Contains(args, required) {
			t.Fatalf("font helper cannot be reclaimed or escaped its runner boundary: missing %q in %s", required, args)
		}
	}
	if strings.Contains(args, source) {
		t.Fatal("font helper received a host path rather than input bytes")
	}
}
