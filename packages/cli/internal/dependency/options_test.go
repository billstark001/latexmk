package dependency

import (
	"reflect"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

func TestBiblatexDataModelDiscovery(t *testing.T) {
	cases := []struct {
		name   string
		source string
		model  string
	}{
		{"single line", `\usepackage[datamodel=datamodels]{biblatex}`, "datamodels"},
		{"issue example", `\usepackage[
  backend      = biber,
  style        = numeric,
  sorting      = none,
  defernumbers = true,
  datamodel    = datamodels,
]{biblatex}`, "datamodels"},
		{"braced whitespace", `\usepackage[ datamodel = { datamodels } , ]{biblatex}`, "datamodels"},
		{
			"comments",
			"\\usepackage% command\n[backend=biber,% option\n datamodel% key\n = data% continuation\n models,% trailing\n]% argument\n{biblatex}",
			"datamodels",
		},
		{"CRLF", "\\usepackage[\r\n datamodel = datamodels,% comment\r\n]{biblatex}", "datamodels"},
		{
			"nested unrelated options",
			`\usepackage[other={x,{y,z}},more={a]b},escaped={\{\}\]},datamodel={datamodels},]{biblatex}`,
			"datamodels",
		},
		{"protected comma", `\usepackage[datamodel={data,models}]{biblatex}`, "data,models"},
		{"protected bracket", `\usepackage[datamodel={data]models}]{biblatex}`, "data]models"},
		{"package list", `\usepackage[datamodel=datamodels]{other,biblatex}`, "datamodels"},
		{"last value", `\usepackage[datamodel=missing,datamodel=datamodels]{biblatex}`, "datamodels"},
		{"root relative path", `\usepackage[datamodel=models/custom]{biblatex}`, "models/custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "main.tex", tc.source)
			writeFile(t, root, tc.model+".dbx", `\input{nested}`)
			writeFile(t, root, "nested.tex", `\input{main}`)
			writeFile(t, root, "unrelated.dbx", "unrelated")
			candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			for _, history := range []bool{false, true} {
				var cached []string
				if history {
					cached = []string{"main.tex", tc.model + ".dbx"}
				}
				result, err := SelectWithOptions("main.tex", candidates, SelectionOptions{
					Mode: "auto", CachedFiles: cached,
				})
				if err != nil {
					t.Fatal(err)
				}
				if !result.Resolved || len(result.Diagnostics) != 0 {
					t.Fatalf("diagnostics = %#v", result.Diagnostics)
				}
				got := map[string]bool{}
				for _, file := range result.Files {
					got[file.Path] = true
					if file.Reason == "" {
						t.Errorf("no selection reason for %s", file.Path)
					}
				}
				want := map[string]bool{"main.tex": true, tc.model + ".dbx": true, "nested.tex": true}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("history=%v: files = %v, want %v", history, got, want)
				}
			}
		})
	}
}

func TestBiblatexDataModelDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, options, kind string }{
		{"missing", "datamodel=missing", "unavailable"},
		{"filtered", "datamodel=private", "unavailable"},
		{"outside", "datamodel=../outside", "outside_root"},
		{"absolute", "datamodel=/outside", "outside_root"},
		{"macro", `datamodel=\model`, "dynamic"},
		{"malformed", "datamodel={model", "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "main.tex", "% header\n\\usepackage["+tc.options+"]{biblatex}")
			writeFile(t, root, "private.dbx", `\input{should-not-read}`)
			writeFile(t, root, "history.tex", "old input")
			candidates, _, err := projectarchive.Manifest(
				projectarchive.Options{Root: root, Exclude: []string{"private.dbx"}},
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Discover("main.tex", candidates)
			if err != nil {
				t.Fatal(err)
			}
			if result.Resolved || len(result.Diagnostics) != 1 || len(result.Files) != 1 {
				t.Fatalf("result = %#v", result)
			}
			diagnostic := result.Diagnostics[0]
			if diagnostic.Kind != tc.kind || diagnostic.File != "main.tex" || diagnostic.Line != 2 ||
				diagnostic.Command != "usepackage" {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
			if tc.kind != "dynamic" {
				cached, err := SelectWithOptions(
					"main.tex",
					candidates,
					SelectionOptions{Mode: "auto", CachedFiles: []string{"history.tex", "private.dbx"}},
				)
				if err != nil {
					t.Fatal(err)
				}
				if cached.Resolved || len(cached.Files) != 2 || cached.Diagnostics[0].Resolution != "" {
					t.Fatalf("history covered a literal failure or bypassed policy: %#v", cached)
				}
			}
		})
	}
}

func TestBiblatexDataModelExclusions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `
% \usepackage[datamodel=missing]{biblatex}
\begin{verbatim}
\usepackage[datamodel=missing]{biblatex}
\end{verbatim}
\verb|\usepackage[datamodel=missing]{biblatex}|
\usepackage[datamodel=missing]{other}
\usepackage[other={datamodel=missing}]{biblatex}
\usepackage[datamodel=missing,datamodel={}]{biblatex}
\usepackage[style=numeric]{biblatex}
\usepackage{biblatex}
`)
	writeFile(t, root, "missing.dbx", "unrelated")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || len(result.Files) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCommentsBetweenInputSyntaxElements(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", "\\input% comment\n{chapter}\n\\includegraphics[width={2]3}]% comment\n{plot}\n")
	writeFile(t, root, "chapter.tex", "chapter")
	writeFile(t, root, "plot.pdf", "plot")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resolved || len(result.Files) != 3 {
		t.Fatalf("result = %#v", result)
	}
}

func TestBiblatexDataModelOptionEditWithHistory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `\usepackage[datamodel=old]{biblatex}`)
	writeFile(t, root, "old.dbx", "old model")
	writeFile(t, root, "new.dbx", "new model")
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	first, err := SelectWithOptions("main.tex", candidates, SelectionOptions{Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Resolved || len(first.Files) != 2 || first.Files[1].Path != "old.dbx" {
		t.Fatalf("first selection = %#v", first)
	}
	writeFile(t, root, "main.tex", `\usepackage[datamodel=new]{biblatex}`)
	candidates, _, err = projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := SelectWithOptions(
		"main.tex",
		candidates,
		SelectionOptions{Mode: "auto", CachedFiles: []string{"main.tex", "old.dbx"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// History is additive, but must not prevent discovery of the newly named model.
	if !changed.Resolved || len(changed.Files) != 3 || changed.Files[1].Path != "new.dbx" {
		t.Fatalf("changed selection = %#v", changed)
	}
}
