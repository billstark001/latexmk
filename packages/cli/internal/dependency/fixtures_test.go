package dependency

import (
	"reflect"
	"sort"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

type discoveryFixture struct {
	name        string
	entry       string
	files       map[string]string
	want        []string
	exclude     []string
	diagnostics []string
}

// Every fixture runs cold, with the previous selection as history, and after
// an entry edit. Exact file assertions also verify that unrelated assets stay out.
func TestRegisteredDependencyFixtures(t *testing.T) {
	fixtures := []discoveryFixture{
		{name: "biblatex style closure", entry: `\usepackage[style=local]{biblatex}`, files: map[string]string{
			"local.bbx": `\RequireBibliographyStyle{base}\DeclareLanguageMapping{english}{english-local}`,
			"base.bbx":  `\RequireBibliographyStyle{numeric}\input{shared}`,
			"local.cbx": `\RequireCitationStyle{base}`, "base.cbx": `\RequireCitationStyle{numeric}`,
			"english-local.lbx": `\InheritBibliographyStrings{english}\InheritBibliographyExtras{english}`,
			"local.dbx":         `\input{model-input}`, "shared.tex": "shared", "model-input.tex": "model",
			"unused.dbx": "unused",
		}, want: []string{"local.bbx", "base.bbx", "local.cbx", "base.cbx", "english-local.lbx", "local.dbx", "shared.tex", "model-input.tex"}},
		{
			name:  "separate styles and implicit models",
			entry: `\RequirePackage[bibstyle=bib,citestyle=cite]{biblatex}`,
			files: map[string]string{
				"bib.bbx":  "",
				"cite.cbx": "",
				"bib.dbx":  "",
				"cite.dbx": "",
				"bib.cbx":  "",
				"cite.bbx": "",
			},
			want: []string{"bib.bbx", "cite.cbx", "bib.dbx", "cite.dbx"},
		},
		{
			name:  "explicit model override and style ordering",
			entry: `\usepackage[style=base,bibstyle=bib,datamodel=custom]{biblatex}`,
			files: map[string]string{
				"base.bbx":   "",
				"base.cbx":   "",
				"base.dbx":   "",
				"bib.bbx":    "",
				"bib.dbx":    "",
				"custom.dbx": "",
			},
			want: []string{"base.cbx", "bib.bbx", "custom.dbx"},
		},
		{
			name:  "style overrides earlier bibstyle",
			entry: `\usepackage[bibstyle=bib,style=base]{biblatex}`,
			files: map[string]string{
				"base.bbx": "",
				"base.cbx": "",
				"base.dbx": "",
				"bib.bbx":  "",
				"bib.dbx":  "",
			},
			want: []string{"base.bbx", "base.cbx", "base.dbx"},
		},
		{
			name:  "forwarded options",
			entry: `\PassOptionsToPackage{datamodel=old}{biblatex}\PassOptionsToPackage{datamodel=new}{biblatex}\usepackage{biblatex}`,
			files: map[string]string{"old.dbx": "", "new.dbx": ""},
			want:  []string{"new.dbx"},
		},
		{
			name:  "forwarded RequirePackage options",
			entry: `\PassOptionsToPackage{datamodel=old}{biblatex}\RequirePackage[datamodel=new]{biblatex}`,
			files: map[string]string{"old.dbx": "", "new.dbx": ""},
			want:  []string{"new.dbx"},
		},
		{name: "forwarding without load", entry: `\PassOptionsToPackage{datamodel=unused}{biblatex}`,
			files: map[string]string{"unused.dbx": ""}},
		{
			name:  "class and package inheritance",
			entry: `\documentclass[datamodel=model]{local}`,
			files: map[string]string{
				"local.cls":    `\LoadClassWithOptions{base}`,
				"base.cls":     `\LoadClass{article}\RequirePackageWithOptions{localpkg}`,
				"localpkg.sty": `\RequirePackageWithOptions{biblatex}\RequirePackage{cycle}`,
				"cycle.sty":    `\RequirePackage{localpkg}\input{nested}`,
				"model.dbx":    "",
				"nested.tex":   "",
			},
			want: []string{"local.cls", "base.cls", "localpkg.sty", "cycle.sty", "model.dbx", "nested.tex"},
		},
		{
			name:  "optional existing input",
			entry: `\InputIfFileExists{local.cfg}{\input{yes}}{\input{missing}}`,
			files: map[string]string{
				"local.cfg":  `\input{nested}`,
				"nested.tex": "",
				"yes.tex":    "",
			},
			want: []string{"local.cfg", "nested.tex", "yes.tex"},
		},
		{name: "optional absent input", entry: `\InputIfFileExists{absent.cfg}{\input{missing}}{\input{no}}`,
			files: map[string]string{"no.tex": ""}, want: []string{"no.tex"}},
		{
			name:  "optional filtered input",
			entry: `\InputIfFileExists{private.cfg}{}{\input{no}}`,
			files: map[string]string{
				"private.cfg": `\input{secret}`,
				"no.tex":      "",
			},
			exclude: []string{"private.cfg"},
			want:    []string{"no.tex"},
		},
		{
			name:  "nested imports",
			entry: `\import{chapters/}{intro}\input{root}`,
			files: map[string]string{
				"chapters/intro.tex":           `\input{body}\subimport{figures/}{diagram}`,
				"chapters/body.tex":            "",
				"body.tex":                     "wrong",
				"chapters/figures/diagram.tex": `\includegraphics{plot}\input{../shared}\input{fallback}`,
				"chapters/figures/plot.pdf":    "",
				"plot.pdf":                     "wrong",
				"chapters/shared.tex":          "",
				"fallback.tex":                 "",
				"root.tex":                     "",
			},
			want: []string{
				"chapters/intro.tex",
				"chapters/body.tex",
				"chapters/figures/diagram.tex",
				"chapters/figures/plot.pdf",
				"chapters/shared.tex",
				"fallback.tex",
				"root.tex",
			},
		},
		{
			name:  "ordinary input remains root relative",
			entry: `\input{chapters/intro}`,
			files: map[string]string{
				"chapters/intro.tex": `\input{body}`,
				"body.tex":           "",
				"chapters/body.tex":  "wrong",
			},
			want: []string{"chapters/intro.tex", "body.tex"},
		},
		{
			name:  "same source in two import contexts",
			entry: `\import{a/}{../shared}\import{b/}{../shared}`,
			files: map[string]string{
				"shared.tex": `\input{body}`,
				"a/body.tex": "",
				"b/body.tex": "",
				"body.tex":   "wrong",
			},
			want: []string{"shared.tex", "a/body.tex", "b/body.tex"},
		},
		{
			name:  "subimport parent normalization",
			entry: `\import{a/b/}{intro}`,
			files: map[string]string{
				"a/b/intro.tex": `\subimport{../c/}{body}`,
				"a/c/body.tex":  "",
			},
			want: []string{"a/b/intro.tex", "a/c/body.tex"},
		},
		{name: "import escapes", entry: `\import{../private/}{body}`, diagnostics: []string{"outside_root"}},
		{
			name:        "missing imported file has no root fallback",
			entry:       `\import{chapters/}{body}`,
			files:       map[string]string{"body.tex": "wrong"},
			diagnostics: []string{"unavailable"},
		},
		{
			name:  "graphics extension order across directories",
			entry: `\graphicspath{{figs/}}\DeclareGraphicsExtensions{.png,.pdf}\includegraphics{plot}`,
			files: map[string]string{"plot.pdf": "wrong", "figs/plot.png": ""},
			want:  []string{"figs/plot.png"},
		},
		{
			name:  "graphics groups and replacement",
			entry: `\graphicspath{{outer/}}{\graphicspath{{inner/}}\DeclareGraphicsExtensions{.png,.pdf}\includegraphics{one}}\includegraphics{two}`,
			files: map[string]string{
				"outer/one.pdf": "wrong",
				"inner/one.png": "",
				"outer/two.pdf": "",
				"inner/two.pdf": "wrong",
			},
			want: []string{"inner/one.png", "outer/two.pdf"},
		},
		{
			name:  "graphics environment scope",
			entry: `\DeclareGraphicsExtensions{.pdf,.png}\begin{figure}\DeclareGraphicsExtensions{.png,.pdf}\includegraphics{one}\end{figure}\includegraphics{two}`,
			files: map[string]string{
				"one.png": "",
				"one.pdf": "wrong",
				"two.png": "wrong",
				"two.pdf": "",
			},
			want: []string{"one.png", "two.pdf"},
		},
		{
			name:  "fonts following options",
			entry: `\setmainfont{Paper-Regular.otf}[Path=fonts/,BoldFont=Paper-Bold.otf,ItalicFont=Paper-Italic.otf]`,
			files: map[string]string{
				"fonts/Paper-Regular.otf": "",
				"fonts/Paper-Bold.otf":    "",
				"fonts/Paper-Italic.otf":  "",
				"fonts/Unused.otf":        "",
			},
			want: []string{"fonts/Paper-Regular.otf", "fonts/Paper-Bold.otf", "fonts/Paper-Italic.otf"},
		},
		{
			name:  "font extension substitution",
			entry: `\setsansfont[Path=fonts/,Extension=.otf,UprightFont=*-Regular,BoldFont=*-Bold]{Paper}\setmonofont{System Mono}`,
			files: map[string]string{
				"fonts/Paper-Regular.otf": "",
				"fonts/Paper-Bold.otf":    "",
				"fonts/Paper.otf":         "wrong",
			},
			want: []string{"fonts/Paper-Regular.otf", "fonts/Paper-Bold.otf"},
		},
		{
			name:  "font family control argument",
			entry: `\newfontfamily\paper[Path=fonts/,Extension=.ttf]{Paper}[ItalicFont=*-Italic]\newfontface{\face}{Other.otf}`,
			files: map[string]string{
				"fonts/Paper.ttf":        "",
				"fonts/Paper-Italic.ttf": "",
				"Other.otf":              "",
			},
			want: []string{"fonts/Paper.ttf", "fonts/Paper-Italic.ttf", "Other.otf"},
		},
		{name: "font named system and explicit face", entry: `\setmainfont{Some System Font}[BoldFont=bold.otf]`,
			files: map[string]string{"bold.otf": "", "Some System Font.otf": "wrong"}, want: []string{"bold.otf"}},
		{
			name:  "font feature defaults and scope",
			entry: `{\defaultfontfeatures{Path=fonts/,Extension=.otf}\setmainfont{Paper}}\setsansfont{System Sans}`,
			files: map[string]string{"fonts/Paper.otf": ""},
			want:  []string{"fonts/Paper.otf"},
		},
		{
			name:  "fontspec companion defaults",
			entry: `\setmainfont{Paper}`,
			files: map[string]string{
				"Paper.fontspec":          `\defaultfontfeatures[Paper]{Path=fonts/,Extension=.otf,UprightFont=*-Regular}`,
				"fonts/Paper-Regular.otf": "",
			},
			want: []string{"Paper.fontspec", "fonts/Paper-Regular.otf"},
		},
		{
			name:  "nested face features",
			entry: `\setmainfont{Paper.otf}[Path=fonts/,BoldFont=Bold.otf,BoldFeatures={Path=other/}]`,
			files: map[string]string{
				"fonts/Paper.otf": "",
				"other/Bold.otf":  "",
				"fonts/Bold.otf":  "wrong",
			},
			want: []string{"fonts/Paper.otf", "other/Bold.otf"},
		},
		{name: "missing font", entry: `\setmainfont[Path=fonts/]{Missing}`, diagnostics: []string{"unavailable"}},
		{
			name:        "outside font",
			entry:       `\setmainfont[Path=../fonts/]{Missing.otf}`,
			diagnostics: []string{"outside_root"},
		},
		{
			name:  "plot literal and data macro",
			entry: `\pgfplotstableread{data/table.csv}\table\addplot+[red] table[x=x,y=y]{data/results.csv};\addplot3 file {data/curve.dat};\addplot table{\table};`,
			files: map[string]string{
				"data/table.csv":   "",
				"data/results.csv": "",
				"data/curve.dat":   "",
			},
			want: []string{"data/table.csv", "data/results.csv", "data/curve.dat"},
		},
		{
			name:  "inline plots are not files",
			entry: "\\addplot table {x y\n1 2\n3 4};\\addplot coordinates {(1,2)(3,4)};\\pgfplotstableread{x y\\\\1 2}\\table",
		},
		{name: "plot dynamic filename", entry: `\addplot file{data/\name.dat};`, diagnostics: []string{"dynamic"}},
		{
			name:  "SVG relative companion closure",
			entry: `\includesvg{figs/diagram}`,
			files: map[string]string{
				"figs/diagram.svg": `<svg xmlns:xlink="http://www.w3.org/1999/xlink"><image href="img%20one.png"/><use xlink:href="symbols.svg#shape"/><image href="https://example.com/remote.png"/><image href="data:image/png;base64,AA=="/><use href="#local"/></svg>`,
				"figs/symbols.svg": `<svg><image href="nested.png"/><use href="diagram.svg#cycle"/></svg>`,
				"figs/img one.png": "",
				"figs/nested.png":  "",
				"img one.png":      "wrong",
			},
			want: []string{"figs/diagram.svg", "figs/symbols.svg", "figs/img one.png", "figs/nested.png"},
		},
		{
			name:        "SVG outside companion",
			entry:       `\includesvg{diagram}`,
			files:       map[string]string{"diagram.svg": `<svg><image href="../secret.png"/></svg>`},
			want:        []string{"diagram.svg"},
			diagnostics: []string{"outside_root"},
		},
		{
			name:  "checked in PDF wrapper",
			entry: `\import{figs/}{diagram.pdf_tex}`,
			files: map[string]string{
				"figs/diagram.pdf_tex": `\includegraphics{diagram.pdf}`,
				"figs/diagram.pdf":     "",
				"diagram.pdf":          "wrong",
			},
			want: []string{"figs/diagram.pdf_tex", "figs/diagram.pdf"},
		},
		{
			name:  "unbraced input comments",
			entry: "\\input chapter.tex\n\\input data% comment\n part.tex\n",
			files: map[string]string{"chapter.tex": "", "datapart.tex": ""},
			want:  []string{"chapter.tex", "datapart.tex"},
		},
		{
			name:  "constant inactive branches",
			entry: `\iffalse\input{missing}\iftrue\input{also-missing}\fi\else\input{yes}\fi\iftrue\input{yes}\else\input{missing}\fi`,
			files: map[string]string{"yes.tex": ""},
			want:  []string{"yes.tex"},
		},
		{
			name:  "unknown branches conservative",
			entry: `\ifdefined\myflag\input{a}\else\input{b}\fi`,
			files: map[string]string{"a.tex": "", "b.tex": ""},
			want:  []string{"a.tex", "b.tex"},
		},
		{
			name:  "filecontents inert until input",
			entry: "\\begin{filecontents*}{example.tex}\n\\input{missing}\n\\end{filecontents*}\n\\input{real}",
			files: map[string]string{"real.tex": ""},
			want:  []string{"real.tex"},
		},
		{
			name:  "generated TeX dependency closure",
			entry: "\\begin{filecontents*}{generated.tex}\n\\input{real}\n\\end{filecontents*}\n\\input{generated}",
			files: map[string]string{"real.tex": ""},
			want:  []string{"real.tex"},
		},
		{
			name:  "generated bibliography not uploaded",
			entry: "\\begin{filecontents*}{refs.bib}\n@book{x,title={T}}\n\\end{filecontents*}\n\\addbibresource{refs.bib}",
		},
		{
			name:  "filecontents keeps existing source",
			entry: "\\begin{filecontents*}{existing.tex}\n\\input{missing}\n\\end{filecontents*}\n\\input{existing}",
			files: map[string]string{"existing.tex": `\input{real}`, "real.tex": ""},
			want:  []string{"existing.tex", "real.tex"},
		},
		{
			name:        "Lua fallback",
			entry:       `\directlua{dofile("data.lua")}`,
			files:       map[string]string{"data.lua": ""},
			diagnostics: []string{"dynamic"},
		},
		{name: "unknown formatting silent", entry: `\textbf{Text}\section{Title}\customwrapper{missing.tex}`},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) { runDiscoveryFixture(t, fixture) })
	}
}

func runDiscoveryFixture(t *testing.T, fixture discoveryFixture) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "main.tex", fixture.entry)
	for name, content := range fixture.files {
		writeFile(t, root, name, content)
	}
	writeFile(t, root, "after-edit.tex", "after edit")
	writeFile(t, root, "unrelated-private.txt", "must not be selected")
	want := append([]string{"main.tex"}, fixture.want...)
	sort.Strings(want)
	var cached []string
	for _, phase := range []string{"cold", "history", "edited"} {
		if phase == "edited" {
			writeFile(t, root, "main.tex", fixture.entry+"\n\\input{after-edit}\n")
			want = append(want, "after-edit.tex")
			sort.Strings(want)
		}
		candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root, Exclude: fixture.exclude})
		if err != nil {
			t.Fatal(err)
		}
		result, err := SelectWithOptions("main.tex", candidates, SelectionOptions{Mode: "auto", CachedFiles: cached})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(result.Files))
		for _, file := range result.Files {
			got = append(got, file.Path)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s files = %v, want %v", phase, got, want)
		}
		kinds := make([]string, 0, len(result.Diagnostics))
		for _, diagnostic := range result.Diagnostics {
			kinds = append(kinds, diagnostic.Kind)
			if diagnostic.Resolution != "" {
				t.Errorf("%s unproven diagnostic coverage: %#v", phase, diagnostic)
			}
		}
		sort.Strings(kinds)
		expected := append([]string{}, fixture.diagnostics...)
		sort.Strings(expected)
		if !reflect.DeepEqual(kinds, expected) || result.Resolved != (len(expected) == 0) {
			t.Errorf("%s diagnostics = %#v, want %v", phase, result.Diagnostics, expected)
		}
		cached = got
	}
}
