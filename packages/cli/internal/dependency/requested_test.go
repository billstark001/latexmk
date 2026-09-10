package dependency

import (
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

func TestResolveRequestedFilesUsesPolicyFilteredCandidates(t *testing.T) {
	candidates := []projectarchive.File{
		{Path: "chapter.tex", Size: 10},
		{Path: "figures/plot.png", Size: 20},
	}
	files, err := ResolveRequestedFiles([]string{"chapter", "figures/plot.png", "chapter"}, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "chapter.tex" || files[1].Path != "figures/plot.png" {
		t.Fatalf("resolved files = %#v", files)
	}
}

func TestResolveRequestedFilesRejectsFilteredAndUnsafePaths(t *testing.T) {
	candidates := []projectarchive.File{{Path: "main.tex"}}
	for _, requested := range []string{".env", "../secret.tex", "/etc/passwd"} {
		if _, err := ResolveRequestedFiles([]string{requested}, candidates); err == nil {
			t.Fatalf("expected %q to be rejected", requested)
		}
	}
}

func TestResolveRequestedFilesRejectsExtensionAmbiguity(t *testing.T) {
	candidates := []projectarchive.File{{Path: "figure.pdf"}, {Path: "figure.png"}}
	_, err := ResolveRequestedFiles([]string{"figure"}, candidates)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestRequestedOptionDependenciesAndAmbiguity(t *testing.T) {
	candidates := []projectarchive.File{
		{Path: "model.dbx"},
		{Path: "font.otf"},
		{Path: "style.bbx"},
		{Path: "style.cbx"},
	}
	files, err := ResolveRequestedFiles([]string{"model", "font"}, candidates)
	if err != nil || len(files) != 2 {
		t.Fatalf("option-driven requests: files=%v err=%v", files, err)
	}
	if _, err := ResolveRequestedFiles(
		[]string{"style"},
		candidates,
	); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("style ambiguity error = %v", err)
	}
	if _, err := ResolveRequestedFiles([]string{"private"}, candidates); err == nil {
		t.Fatal("missing request unexpectedly accepted")
	}
}
