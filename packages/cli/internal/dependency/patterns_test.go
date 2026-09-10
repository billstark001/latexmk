package dependency

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

func TestDependencyPatternsRespectFilteredManifest(t *testing.T) {
	for _, pattern := range []struct{ name, entry, file string }{
		{"explicit model", `\usepackage[datamodel=sub/private]{biblatex}`, "sub/private.dbx"},
		{"inherited bibliography style", `\RequireBibliographyStyle{sub/private}`, "sub/private.bbx"},
		{"inherited citation style", `\RequireCitationStyle{sub/private}`, "sub/private.cbx"},
		{"language mapping", `\DeclareLanguageMapping{english}{sub/private}`, "sub/private.lbx"},
		{"local class", `\LoadClass{sub/private}`, "sub/private.cls"},
		{"local package", `\RequirePackage{sub/private}`, "sub/private.sty"},
		{"import", `\import{sub/}{private}`, "sub/private.tex"},
		{"font", `\setmainfont[Path=sub/]{private.otf}`, "sub/private.otf"},
		{"table", `\addplot table{sub/private.csv};`, "sub/private.csv"},
		{"file plot", `\addplot file{sub/private.dat};`, "sub/private.dat"},
		{"graphics", `\DeclareGraphicsExtensions{.png,.pdf}\includegraphics{sub/private}`, "sub/private.png"},
		{"SVG", `\includesvg{sub/private}`, "sub/private.svg"},
		{"unbraced input", `\input sub/private.tex `, "sub/private.tex"},
	} {
		t.Run(pattern.name, func(t *testing.T) {
			for _, policy := range []string{"missing", "excluded", "symlink"} {
				t.Run(policy, func(t *testing.T) {
					root := t.TempDir()
					writeFile(t, root, "main.tex", pattern.entry)
					options := projectarchive.Options{Root: root}
					if policy == "excluded" {
						writeFile(t, root, pattern.file, `\input{private-secret}`)
						options.Exclude = []string{pattern.file}
					}
					if policy == "symlink" {
						outside := filepath.Join(t.TempDir(), "private")
						if err := os.WriteFile(outside, []byte(`\input{private-secret}`), 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.MkdirAll(filepath.Dir(filepath.Join(root, pattern.file)), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(outside, filepath.Join(root, pattern.file)); err != nil {
							t.Skipf("symlinks unavailable: %v", err)
						}
					}
					candidates, _, err := projectarchive.Manifest(options)
					if policy == "symlink" {
						if err == nil || !strings.Contains(err.Error(), "symlinks are not supported") {
							t.Fatalf("symlink manifest error = %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					result, err := SelectWithOptions(
						"main.tex",
						candidates,
						SelectionOptions{Mode: "auto", CachedFiles: []string{pattern.file}},
					)
					if err != nil {
						t.Fatal(err)
					}
					if result.Resolved || len(result.Files) != 1 || len(result.Diagnostics) != 1 ||
						result.Diagnostics[0].Kind != "unavailable" {
						t.Fatalf("filtered result = %#v", result)
					}
				})
			}
		})
	}
}

func TestAdditionalParserAndAssetFixtures(t *testing.T) {
	for _, fixture := range []discoveryFixture{
		{name: "optional hook precedes file", entry: `\InputIfFileExists{local.cfg}{\graphicspath{{figs/}}}{}`,
			files: map[string]string{"local.cfg": `\includegraphics{plot}`, "figs/plot.pdf": ""}, want: []string{"local.cfg", "figs/plot.pdf"}},
		{name: "generated overwrite", entry: "\\begin{filecontents*}[overwrite]{generated.tex}\n\\input{real}\n\\end{filecontents*}\n\\input{generated}", files: map[string]string{"generated.tex": `\input{missing}`, "real.tex": ""}, want: []string{"real.tex"}},
		{name: "generated SVG preserves percent", entry: "\\begin{filecontents*}{generated.svg}\n<svg><image href=\"img%20one.png\"/></svg>\n\\end{filecontents*}\n\\includesvg{generated}", files: map[string]string{"img one.png": ""}, want: []string{"img one.png"}},
		{name: "SVG checked in export", entry: `\usepackage[inkscape=false]{svg}\includesvg{figs/source}`,
			files: map[string]string{"figs/source.svg": "<svg/>", "svg-inkscape/source_svg-tex.pdf": "", "svg-inkscape/source_svg-tex.pdf_tex": `\includegraphics{source_svg-tex.pdf}`}, want: []string{"figs/source.svg", "svg-inkscape/source_svg-tex.pdf", "svg-inkscape/source_svg-tex.pdf_tex"}},
		{name: "SVG disabled export missing", entry: `\includesvg[inkscape=false,inkscapelatex=false]{source}`,
			files: map[string]string{"source.svg": "<svg/>"}, want: []string{"source.svg"}, diagnostics: []string{"unavailable"}},
		{name: "SVG explicit export path", entry: `\svgsetup{inkscape=false,inkscapelatex=false,inkscapepath=export/,inkscapename=renamed}\svgpath{{figs/}}\includesvg{source}`,
			files: map[string]string{"figs/source.svg": "<svg/>", "export/renamed_svg-raw.pdf": ""}, want: []string{"figs/source.svg", "export/renamed_svg-raw.pdf"}},
		{name: "SVG source relative export", entry: `\includesvg[inkscape=false,inkscapelatex=false,inkscapepath=svgdir]{figs/source}`,
			files: map[string]string{"figs/source.svg": "<svg/>", "figs/source_svg-raw.pdf": ""}, want: []string{"figs/source.svg", "figs/source_svg-raw.pdf"}},
		{name: "SVG filtered raster", entry: `\includesvg{source}`, files: map[string]string{"source.svg": `<svg><image href="private.png"/></svg>`, "private.png": ""}, exclude: []string{"private.png"}, want: []string{"source.svg"}, diagnostics: []string{"unavailable"}},
		{name: "SVG file URI refused", entry: `\includesvg{source}`, files: map[string]string{"source.svg": `<svg><image href="file:///etc/passwd"/></svg>`}, want: []string{"source.svg"}, diagnostics: []string{"outside_root"}},
		{name: "verbatim syntax elements", entry: "\\begin% comment\n{verbatim}\n\\usepackage[datamodel=missing]{biblatex}\n\\end{verbatim}"},
		{name: "escaped delimiters", entry: `\usepackage[other={\{a\}\]b},datamodel={model}]{biblatex}`, files: map[string]string{"model.dbx": ""}, want: []string{"model.dbx"}},
		{name: "style inheritance cycle", entry: `\usepackage[style=local]{biblatex}`, files: map[string]string{"local.bbx": `\RequireBibliographyStyle{base}`, "base.bbx": `\RequireBibliographyStyle{local}`, "local.cbx": ""}, want: []string{"local.bbx", "base.bbx", "local.cbx"}},
	} {
		t.Run(fixture.name, func(t *testing.T) { runDiscoveryFixture(t, fixture) })
	}
}

func TestCompanionTraversalIsBounded(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.tex", `\includesvg{0}`)
	for i := 0; i < 258; i++ {
		writeFile(t, root, fmt.Sprintf("%d.svg", i), fmt.Sprintf(`<svg><use href="%d.svg"/></svg>`, i+1))
	}
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Discover("main.tex", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved || len(result.Diagnostics) != 1 || result.Diagnostics[0].Kind != "limit" {
		t.Fatalf("result = %#v", result.Diagnostics)
	}
}

func TestConditionalDependencySettingsRemainUnresolved(t *testing.T) {
	runDiscoveryFixture(t, discoveryFixture{
		name:  "conditional settings",
		entry: "\\ifdefined\\flag\n\\graphicspath{{a/}}\n\\else\n\\graphicspath{{b/}}\n\\fi\n\\includegraphics{plot}",
		files: map[string]string{"a/plot.pdf": "", "b/plot.pdf": ""},
		want:  []string{"b/plot.pdf"}, diagnostics: []string{"dynamic", "dynamic"},
	})
}

func TestSVGBaseOverrideIsNotSilentlyResolved(t *testing.T) {
	runDiscoveryFixture(t, discoveryFixture{
		name:  "XML base override",
		entry: `\includesvg{source}`,
		files: map[string]string{
			"source.svg":      `<svg xml:base="nested/"><image href="plot.png"/></svg>`,
			"plot.png":        "",
			"nested/plot.png": "",
		},
		want:        []string{"source.svg"},
		diagnostics: []string{"unsupported"},
	})
}

func TestDynamicFontAndSVGOptions(t *testing.T) {
	runDiscoveryFixture(
		t,
		discoveryFixture{
			name:        "dynamic face",
			entry:       `\setmainfont{System Font}[BoldFont=\boldface]`,
			diagnostics: []string{"dynamic"},
		},
	)
	runDiscoveryFixture(
		t,
		discoveryFixture{
			name:        "dynamic SVG export",
			entry:       `\includesvg[inkscapelatex=\setting]{source}`,
			files:       map[string]string{"source.svg": "<svg/>"},
			want:        []string{"source.svg"},
			diagnostics: []string{"dynamic"},
		},
	)
}
