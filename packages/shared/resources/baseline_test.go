package resources

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func baselineDigestFixture() Baseline {
	return Baseline{
		ID:          "paper-runtime",
		Image:       "registry.example/runtime@sha256:" + strings.Repeat("a", 64),
		TexliveYear: 2026,
		Repository:  "https://snapshot.example/2026/09/09/tlnet",
		Engines:     []string{"xelatex", "lualatex"},
		FontLookup:  "names",
		Toolchain:   map[string]string{"xelatex": "XeTeX 2026", "biber": "Biber 2.21"},
		Fonts: []Font{
			{
				File:       "Paper-Regular.otf",
				SHA256:     strings.Repeat("b", 64),
				Index:      0,
				Family:     "Paper",
				Style:      "Regular",
				PostScript: "Paper-Regular",
			},
			{
				File:       "Paper-Bold.otf",
				SHA256:     strings.Repeat("c", 64),
				Index:      0,
				Family:     "Paper",
				Style:      "Bold",
				PostScript: "Paper-Bold",
			},
		},
		Packages: []Package{
			{Name: "algorithms", Revision: 42, Checksum: strings.Repeat("d", 128)},
			{Name: "xcolor", Revision: 43, Checksum: strings.Repeat("e", 128)},
		},
	}
}

func cloneBaselineDigestFixture(t *testing.T, baseline Baseline) Baseline {
	t.Helper()
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	var result Baseline
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestBaselineDigestCanonicalizesOrderDuplicatesAndOldDigestWithoutMutation(t *testing.T) {
	baseline := baselineDigestFixture()
	baseline.Digest = strings.Repeat("1", 64)
	canonical, err := BaselineDigest(baseline)
	if err != nil {
		t.Fatal(err)
	}
	variant := cloneBaselineDigestFixture(t, baseline)
	variant.Digest = strings.Repeat("2", 64)
	variant.Engines = []string{"lualatex", "xelatex", "lualatex", "xelatex"}
	variant.Fonts = []Font{baseline.Fonts[1], baseline.Fonts[0], baseline.Fonts[1], baseline.Fonts[0]}
	variant.Packages = []Package{baseline.Packages[1], baseline.Packages[0], baseline.Packages[1], baseline.Packages[0]}
	variant.Toolchain = map[string]string{"biber": "Biber 2.21", "xelatex": "XeTeX 2026"}
	before := cloneBaselineDigestFixture(t, variant)
	actual, err := BaselineDigest(variant)
	if err != nil || actual != canonical {
		t.Fatalf("equivalent runtime inventories changed identity: %s versus %s, %v", actual, canonical, err)
	}
	if !reflect.DeepEqual(variant, before) {
		t.Fatalf("digesting changed caller-owned input: %#v versus %#v", variant, before)
	}
	variant.Digest = ""
	withoutOldDigest, err := BaselineDigest(variant)
	if err != nil || withoutOldDigest != canonical {
		t.Fatalf("an existing digest participated in its own identity: %s %v", withoutOldDigest, err)
	}
}

func TestBaselineDigestIncludesEveryRuntimeIdentityField(t *testing.T) {
	baseline := baselineDigestFixture()
	want, err := BaselineDigest(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Baseline)
	}{
		{"id", func(value *Baseline) { value.ID = "another-runtime" }},
		{"image", func(value *Baseline) { value.Image = "registry.example/runtime@sha256:" + strings.Repeat("f", 64) }},
		{"texlive-year", func(value *Baseline) { value.TexliveYear++ }},
		{"repository", func(value *Baseline) { value.Repository = "https://snapshot.example/2026/10/10/tlnet" }},
		{"engines", func(value *Baseline) { value.Engines = append(value.Engines, "pdflatex") }},
		{"toolchain-version", func(value *Baseline) { value.Toolchain["xelatex"] = "another XeTeX build" }},
		{"toolchain-presence", func(value *Baseline) { delete(value.Toolchain, "biber") }},
		{"font-lookup", func(value *Baseline) { value.FontLookup = "files" }},
		{"font-file", func(value *Baseline) { value.Fonts[0].File = "Another-Regular.otf" }},
		{"font-checksum", func(value *Baseline) { value.Fonts[0].SHA256 = strings.Repeat("f", 64) }},
		{"font-index", func(value *Baseline) { value.Fonts[0].Index++ }},
		{"font-family", func(value *Baseline) { value.Fonts[0].Family = "Another Family" }},
		{"font-style", func(value *Baseline) { value.Fonts[0].Style = "Italic" }},
		{"font-postscript-name", func(value *Baseline) { value.Fonts[0].PostScript = "Another-Regular" }},
		{"package-name", func(value *Baseline) { value.Packages[0].Name = "algorithmicx" }},
		{"package-revision", func(value *Baseline) { value.Packages[0].Revision++ }},
		{"package-checksum", func(value *Baseline) { value.Packages[0].Checksum = strings.Repeat("f", 128) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := cloneBaselineDigestFixture(t, baseline)
			test.mutate(&changed)
			got, err := BaselineDigest(changed)
			if err != nil || got == want {
				t.Fatalf("changed %s did not produce a different runtime identity: %s %v", test.name, got, err)
			}
		})
	}
}
