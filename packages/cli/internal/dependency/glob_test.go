package dependency

import (
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

func TestGlobSelectionAndNoMatchPolicies(t *testing.T) {
	candidates := []projectarchive.File{
		{Path: "main.tex"},
		{Path: "sections/a.tex"},
		{Path: "sections/deep/b.tex"},
		{Path: "fig/a.pdf"},
	}
	result, err := SelectWithOptions(
		"main.tex",
		candidates,
		SelectionOptions{Mode: "manifest", ExplicitFiles: []string{"sections/**/*.tex", "sections/a.tex"}},
	)
	if err != nil || !result.Resolved || len(result.Files) != 3 {
		t.Fatalf("glob expansion: %+v %v", result, err)
	}
	for _, policy := range []string{"error", "warn", "ignore"} {
		result, err := SelectWithOptions(
			"main.tex",
			candidates,
			SelectionOptions{Mode: "manifest", ExplicitFiles: []string{"missing/**/*.tex"}, UnmatchedGlob: policy},
		)
		if err != nil || result.Resolved != (policy != "error") {
			t.Fatalf("no-match %s: %+v %v", policy, result, err)
		}
	}
	for _, pattern := range []string{"../*.tex", "/tmp/*.tex", "a/../*.tex", "[bad"} {
		if _, err := SelectWithOptions(
			"main.tex",
			candidates,
			SelectionOptions{Mode: "manifest", ExplicitFiles: []string{pattern}},
		); err == nil {
			t.Fatalf("accepted %s", pattern)
		}
	}
}

func TestMissingEscapedLiteralIsAlwaysAnError(t *testing.T) {
	result, err := SelectWithOptions("main.tex", []projectarchive.File{{Path: "main.tex"}}, SelectionOptions{
		Mode: "manifest", ExplicitFiles: []string{ExactPattern("missing*.tex")}, UnmatchedGlob: "ignore",
	})
	if err != nil || result.Resolved {
		t.Fatalf("missing literal treated as optional glob: %+v %v", result, err)
	}
}

func TestCommentContinuationKeepsPathAndLine(t *testing.T) {
	root := t.TempDir()
	writeFile(
		t,
		root,
		"main.tex",
		"\\graphicspath{{figures/% comment\n }}\n\\includegraphics{%\n image with spaces.pdf}\n\\input{missing}\n",
	)
	writeFile(t, root, "figures/image with spaces.pdf", "pdf")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil || len(result.Files) != 2 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Line != 5 {
		t.Fatalf("continuation: %+v %v", result, err)
	}
}
