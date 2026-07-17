package dependency

import (
	"os"
	"path/filepath"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

func TestDiscoverBuildsLiteralDependencyClosure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `
\documentclass{article}
\usepackage{graphicx,localpkg}
\graphicspath{{figures/}}
\input{sections/body}
\includegraphics{plot}
\addbibresource{refs.bib}
\lstinputlisting{code/example.py}
% \input{private/commented}
\begin{verbatim}
\input{private/verbatim}
\end{verbatim}
\verb|\input{private/inline}|
`)
	writeFile(t, root, "sections/body.tex", `\input{shared}`)
	writeFile(t, root, "shared.tex", "shared")
	writeFile(t, root, "localpkg.sty", `\input{package-data.tex}`)
	writeFile(t, root, "package-data.tex", "package data")
	writeFile(t, root, "figures/plot.pdf", "pdf")
	writeFile(t, root, "refs.bib", "bib")
	writeFile(t, root, "code/example.py", "print('ok')")
	writeFile(t, root, "private/commented.tex", "secret")
	writeFile(t, root, "private/verbatim.tex", "secret")
	writeFile(t, root, "private/inline.tex", "secret")
	writeFile(t, root, "unrelated-secret.txt", "secret")

	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || len(result.Diagnostics) != 0 {
		t.Fatalf("discovery incomplete: %#v", result.Diagnostics)
	}
	got := make(map[string]projectarchive.File)
	for _, file := range result.Files {
		got[file.Path] = file
	}
	for _, expected := range []string{
		"main.tex", "sections/body.tex", "shared.tex", "localpkg.sty",
		"package-data.tex", "figures/plot.pdf", "refs.bib", "code/example.py",
	} {
		if _, ok := got[expected]; !ok {
			t.Errorf("dependency closure missing %q: %#v", expected, got)
		}
	}
	for _, unwanted := range []string{"private/commented.tex", "private/verbatim.tex", "private/inline.tex", "unrelated-secret.txt"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("dependency closure included unrelated %q", unwanted)
		}
	}
	if got["figures/plot.pdf"].Reason == "" {
		t.Fatal("selected dependency is missing an explanation")
	}
}

func TestDiscoverReportsDynamicMissingAndOutsideReferences(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", "\\input{\\chapterfile}\n\\includegraphics{missing}\n\\input{../outside}\n")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved {
		t.Fatal("expected unresolved dependencies")
	}
	kinds := make(map[string]bool)
	for _, diagnostic := range result.Diagnostics {
		kinds[diagnostic.Kind] = true
	}
	if !kinds["dynamic"] || !kinds["unavailable"] || !kinds["outside_root"] {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
}

func TestDiscoverDoesNotTreatSystemPackagesAsExtensionlessLocalFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", "\\documentclass{article}\n\\usepackage{graphicx}\n")
	writeFile(t, root, "article", "unrelated private file")
	writeFile(t, root, "graphicx", "unrelated private file")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || len(result.Files) != 1 || result.Files[0].Path != "main.tex" {
		t.Fatalf("system package result = %#v", result)
	}
}

func TestDiscoverDoesNotOverridePolicyFilteredManifest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `\input{private}`)
	writeFile(t, root, "private.tex", "secret")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root, Exclude: []string{"private.tex"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved || len(result.Files) != 1 || result.Files[0].Path != "main.tex" {
		t.Fatalf("policy-filtered result = %#v", result)
	}
}

func TestDiscoverHandlesCyclicInputs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `\input{other}`)
	writeFile(t, root, "other.tex", `\input{main}`)
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || len(result.Files) != 2 {
		t.Fatalf("cyclic result = %#v", result)
	}
}

func TestSelectAllKeepsEveryPolicyAllowedCandidate(t *testing.T) {
	candidates := []projectarchive.File{{Path: "main.tex", Size: 4}, {Path: "notes.txt", Size: 5}}
	result, err := Select("main.tex", "all", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || result.Stats.Files != 2 || result.Stats.Bytes != 9 {
		t.Fatalf("all result = %#v", result)
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
