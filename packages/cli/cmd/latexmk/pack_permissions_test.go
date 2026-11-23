package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"runtime"
	"testing"
)

func TestPackPermissionsDefaultRecipeAndExplicitOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX permission metadata")
	}
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writePublicationFiles(t, map[string]string{
		".latexmk.json":       `{"uploadMode":"all","token":{"file":"missing-token"},"resources":{"tool":{"type":"files","kind":"data","path":"private/tool","mount":"tools","redistribution":"allow"}},"targets":{"paper":{"entry":"main.tex","resources":["tool"]}},"publications":{"release":{"target":"paper","verify":"none","preservePermissions":true,"resources":{"tool":"bundle"},"output":"release.zip"}}}`,
		"main.tex":            "source",
		"private/tool/run.sh": "tool",
		"helper.sh":           "helper",
	})
	for name, mode := range map[string]os.FileMode{"main.tex": 0640, "helper.sh": 0755, "private/tool/run.sh": 0750} {
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name     string
		args     []string
		preserve bool
		resource bool
	}{
		{"default", []string{"main.tex", "--output=default.zip"}, false, false},
		{"explicit", []string{"main.tex", "--output=explicit.zip", "--preserve-permissions"}, true, false},
		{"recipe", []string{"--publication=release"}, true, true},
		{"override", []string{"--publication=release", "--preserve-permissions=false", "--output=override.zip"}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"latexmk", "pack", "--json"}, test.args...)
			code, stdout, stderr := captureCommandOutput(t, func() int { return run(args) })
			if code != 0 {
				t.Fatalf("pack failed: %d %s %s", code, stdout, stderr)
			}
			var envelope struct{ Data packView }
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatal(err)
			}
			view := envelope.Data
			t.Cleanup(func() {
				if err := os.Remove(view.Output); err != nil {
					t.Error(err)
				}
			})
			if view.PreservePermissions != test.preserve || view.Plan.PreservePermissions != test.preserve {
				t.Fatalf("wrong reported permission request: %+v", view)
			}
			archive, err := zip.OpenReader(view.Output)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = archive.Close() }()
			seenResource := false
			for _, file := range archive.File {
				want := os.FileMode(0644)
				if test.preserve {
					switch file.Name {
					case "main.tex":
						want = 0640
					case "helper.sh":
						want = 0755
					case "tools/run.sh":
						want = 0750
					}
				}
				if file.Name == "tools/run.sh" {
					seenResource = true
				}
				if file.Mode() != want || !file.Mode().IsRegular() {
					t.Fatalf("%s got %v want %v", file.Name, file.Mode(), want)
				}
			}
			if seenResource != test.resource {
				t.Fatalf("resource bundle differs from publication policy: %v", seenResource)
			}
		})
	}
}
