package config

import (
	"encoding/json"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/resources"
)

func TestTargetPreservesExplicitEmptyResourceOverride(t *testing.T) {
	original := Target{Entry: "main.tex", Resources: []string{}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Target
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Resources == nil {
		t.Fatalf("explicit clear lost: %s", data)
	}
}
func TestResourceConfigurationRejectsUnknownReferencesAndUnsafePaths(t *testing.T) {
	for _, cfg := range []FileConfig{{DefaultResources: []string{"missing"}}, {Resources: map[string]resources.Spec{"font": {Type: "files", Path: "../font.otf"}}}, {Resources: map[string]resources.Spec{"font": {Type: "files", Path: "fonts", Mount: ".latexmk-home"}}}, {Publications: map[string]resources.Publication{"release": {Target: "missing"}}}} {
		if err := validateResources(cfg); err == nil {
			t.Fatalf("invalid resource configuration accepted: %#v", cfg)
		}
	}
}
