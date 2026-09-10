package dependency

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

// Opt in against a real service. Fonts are supplied by the operator so the
// repository need not vendor font binaries. This test never changes deployment.
func TestCompilerDependencyFixture(t *testing.T) {
	server := os.Getenv("LATEXMK_DEPENDENCY_SERVER")
	if server == "" {
		t.Skip(
			"set LATEXMK_DEPENDENCY_SERVER, LATEXMK_DEPENDENCY_TOKEN_FILE and LATEXMK_DEPENDENCY_FONT_DIR for compiler validation",
		)
	}
	cli, err := filepath.Abs("../../dist/latexmk")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if parent := os.Getenv("LATEXMK_DEPENDENCY_VALIDATION_DIR"); parent != "" {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		root, err = os.MkdirTemp(parent, "compiler-")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("validation artifacts: %s", root)
	}
	err = fs.WalkDir(os.DirFS("testdata/compiler"), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(filepath.Join("testdata/compiler", name))
		if err != nil {
			return err
		}
		writeFile(t, root, name, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, face := range []string{"Regular", "Bold"} {
		font, err := os.ReadFile(filepath.Join(os.Getenv("LATEXMK_DEPENDENCY_FONT_DIR"), "Paper-"+face+".otf"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "fonts/Paper-"+face+".otf", string(font))
	}
	var raster bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 100, B: 180, A: 255})
		}
	}
	if err := png.Encode(&raster, img); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "figures/ordered.png", raster.String())
	writeFile(t, root, "figures/ordered.pdf", minimalPDF())
	writeFile(t, root, "figures/diagram.pdf", minimalPDF())
	writeFile(t, root, "svg-inkscape/source_svg-raw.pdf", minimalPDF())
	writeFile(t, root, "unrelated.tex", "unrelated")
	writeFile(t, root, "after-edit.tex", "New dependency after a successful compile.")
	var history []string
	for _, phase := range []string{"cold", "history", "edited"} {
		if phase == "edited" {
			entry, err := os.ReadFile(filepath.Join(root, "main.tex"))
			if err != nil {
				t.Fatal(err)
			}
			writeFile(
				t,
				root,
				"main.tex",
				strings.Replace(
					string(entry),
					`\input root-input.tex`,
					`\input root-input.tex`+"\n\\input{after-edit}",
					1,
				),
			)
		}
		candidates, _, err := projectarchive.Manifest(
			projectarchive.Options{Root: root, Exclude: []string{"results/**", "evidence/**"}},
		)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := SelectWithOptions(
			"main.tex",
			candidates,
			SelectionOptions{Mode: "auto", CachedFiles: history},
		)
		if err != nil {
			t.Fatal(err)
		}
		if !selection.Resolved {
			t.Fatalf("%s static selection: %#v", phase, selection.Diagnostics)
		}
		selected := map[string]bool{}
		for _, file := range selection.Files {
			selected[file.Path] = true
		}
		if selected["unrelated.tex"] || selected["figures/ordered.pdf"] ||
			selected["after-edit.tex"] != (phase == "edited") {
			t.Fatalf("%s extra/missing static files: %v", phase, selected)
		}
		payload, err := json.MarshalIndent(selection, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "evidence/"+phase+"-selection.json", string(payload))
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		args := []string{
			"--server",
			server,
			"--token-file",
			os.Getenv("LATEXMK_DEPENDENCY_TOKEN_FILE"),
			"--engine",
			"xelatex",
			"--project-root",
			root,
			"--out-dir",
			"results/" + phase,
			"--ignore-file",
			".latexmkignore",
			"--local-cache",
			"output",
			"--server-cache",
			"reuse",
			"--timeout",
			"3m",
			"--json",
			"main.tex",
		}
		writeFile(t, root, ".latexmkignore", "results/\nevidence/\n")
		command := exec.CommandContext(ctx, cli, args...)
		command.Dir = root
		var stderr bytes.Buffer
		command.Stderr = &stderr
		stdout, runErr := command.Output()
		cancel()
		writeFile(t, root, "evidence/"+phase+"-result.json", string(stdout))
		writeFile(t, root, "evidence/"+phase+"-stderr.txt", stderr.String())
		if runErr != nil {
			t.Fatalf("%s compile: %v\n%s\n%s", phase, runErr, stdout, stderr.String())
		}
		var result struct {
			Success    bool
			ExitCode   int
			RequestID  string
			InputFiles []string
		}
		if err := json.Unmarshal(stdout, &result); err != nil {
			t.Fatal(err)
		}
		if !result.Success || result.ExitCode != 0 {
			t.Fatalf("%s compile unsuccessful: %s", phase, stdout)
		}
		candidateSet := map[string]bool{}
		for _, file := range candidates {
			candidateSet[file.Path] = true
		}
		for _, file := range result.InputFiles {
			if candidateSet[file] && !selected[file] {
				t.Errorf("%s compiler input missing from static selection: %s", phase, file)
			}
		}
		if !containsString(result.InputFiles, "figures/ordered.png") {
			t.Errorf("%s compiler did not use the declared graphics extension order: %v", phase, result.InputFiles)
		}
		blg, err := os.ReadFile(filepath.Join(root, "results", phase, "main.blg"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(blg), "refs.bib") {
			t.Error("Biber log does not confirm the bibliography input")
		}
		t.Logf(
			"%s: job=%s selected=%d recorder=%d bibliography-in-recorder=%v",
			phase,
			result.RequestID,
			len(selected),
			len(result.InputFiles),
			containsString(result.InputFiles, "refs.bib"),
		)
		history = result.InputFiles
	}
}

func minimalPDF() string {
	var out strings.Builder
	out.WriteString("%PDF-1.4\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 20 20] /Resources << >> /Contents 4 0 R >>",
		"<< /Length 4 >>\nstream\nq\nQ\nendstream",
	}
	offsets := []int{0}
	for i, object := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return out.String()
}
