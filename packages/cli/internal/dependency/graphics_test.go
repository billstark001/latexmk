package dependency

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/engine"
)

func TestEngineGraphicsSelection(t *testing.T) {
	for _, test := range []struct {
		name, engine, source string
		files, want          []string
	}{
		{"pdf prefers PNG over uppercase PDF", "pdflatex", `\includegraphics{plot}`, []string{"plot.PDF", "plot.png"}, []string{"main.tex", "plot.png"}},
		{"lua uses pdf driver order", "lualatex", `\includegraphics{plot}`, []string{"plot.PDF", "plot.png"}, []string{"main.tex", "plot.png"}},
		{"xe prefers uppercase PDF over PNG", "xelatex", `\includegraphics{plot}`, []string{"plot.PDF", "plot.png"}, []string{"main.tex", "plot.PDF"}},
		{"pdf prefers MPS over JPEG", "pdflatex", `\includegraphics{plot}`, []string{"plot.mps", "plot.jpeg"}, []string{"main.tex", "plot.mps"}},
		{"xe prefers JPEG over MPS", "xelatex", `\includegraphics{plot}`, []string{"plot.mps", "plot.jpeg"}, []string{"main.tex", "plot.jpeg"}},
		{"pdf uppercase JPEG", "pdflatex", `\includegraphics{plot}`, []string{"plot.JPEG"}, []string{"main.tex", "plot.JPEG"}},
		{"pdf JBIG2", "pdflatex", `\includegraphics{plot}`, []string{"plot.jbig2"}, []string{"main.tex", "plot.jbig2"}},
		{"xe bitmap", "xelatex", `\includegraphics{plot}`, []string{"plot.BMP"}, []string{"main.tex", "plot.BMP"}},
		{"declared order overrides driver", "pdflatex", `\DeclareGraphicsExtensions{.JPEG,.pdf}\includegraphics{plot}`, []string{"plot.JPEG", "plot.pdf"}, []string{"main.tex", "plot.JPEG"}},
		{"declared order restores after group", "pdflatex", `{\DeclareGraphicsExtensions{.PDF}\includegraphics{plot}}\includegraphics{plot}`, []string{"plot.PDF", "plot.png"}, []string{"main.tex", "plot.PDF", "plot.png"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "main.tex", test.source)
			for _, name := range test.files {
				writeFile(t, root, name, "test graphic")
			}
			candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			result, err := SelectWithOptions(
				"main.tex",
				candidates,
				SelectionOptions{Mode: "auto", Engine: test.engine},
			)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, file := range result.Files {
				paths = append(paths, file.Path)
			}
			if !result.Resolved || !reflect.DeepEqual(paths, test.want) {
				t.Fatalf("selection = %v, diagnostics = %v; want %v", paths, result.Diagnostics, test.want)
			}
		})
	}
}

func TestEngineGraphicsNeverSelectsUnsupportedDefault(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `\includegraphics{plot}`)
	writeFile(t, root, "plot.bmp", "XeTeX-only graphic")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", "pdflatex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved || len(result.Files) != 1 || len(result.Diagnostics) != 1 {
		t.Fatalf("unexpected pdfLaTeX selection: %#v", result)
	}
	for _, engine := range []string{"", "unknown"} {
		if _, err := Discover("main.tex", engine, candidates); err == nil {
			t.Fatalf("unknown engine %q used a fallback", engine)
		}
	}
}

var customEngineCounter atomic.Uint64

type chartEngine struct{}

func (chartEngine) LatexmkArgs() []string        { return []string{"-pdf"} }
func (chartEngine) GraphicsExtensions() []string { return []string{".chart", ".pdf"} }
func (chartEngine) VersionProbe() engine.Command {
	return engine.Command{Name: "chart-tex", Args: []string{"--version"}}
}

func TestRegisteredEngineControlsDiscoveryAndRequestedExtensions(t *testing.T) {
	name := fmt.Sprintf("lab/chart+%d", customEngineCounter.Add(1))
	if err := engine.Default.Register(name, chartEngine{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeFile(t, root, "main.tex", `\includegraphics{plot}`)
	writeFile(t, root, "plot.chart", "chart")
	writeFile(t, root, "plot.pdf", "PDF")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", name, candidates)
	if err != nil || !result.Resolved || len(result.Files) != 2 || result.Files[1].Path != "plot.chart" {
		t.Fatalf("custom engine selection: %#v %v", result, err)
	}
	for i, file := range candidates {
		if file.Path == "plot.pdf" {
			candidates = append(candidates[:i], candidates[i+1:]...)
			break
		}
	}
	requested, err := ResolveRequestedFiles([]string{"plot"}, candidates)
	if err != nil || len(requested) != 1 || requested[0].Path != "plot.chart" {
		t.Fatalf("registered extension missing from bounded recovery: %v %v", requested, err)
	}
}
