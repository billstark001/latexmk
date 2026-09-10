package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreSourcesWithoutGitAndHardDeny(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"sections/deep", "other"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string]string{
		".gitignore":     "*.tmp\nsections/**\n!sections/\n!sections/**/*.tex\n",
		".latexmkignore": "other/\n!.env.latexmk\n",
		".env.latexmk":   "SECRET=value", ".latexmk-token": "secret",
		"main.tex": "main", "sections/deep/body.tex": "body", "sections/deep/data.tmp": "ignored", "other/file.tex": "ignored",
	} {
		mustWrite(t, filepath.Join(root, name), data)
	}
	files, _, err := Manifest(Options{Root: root, RespectGitIgnore: true, Exclude: []string{".gitignore"}})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, file := range files {
		names[file.Path] = true
	}
	if !names["main.tex"] || names[".env.latexmk"] || names[".latexmk-token"] || names["other/file.tex"] ||
		names["sections/deep/data.tmp"] {
		t.Fatalf("policy: %v", names)
	}
	files, _, err = Manifest(
		Options{Root: root, IgnoreFiles: []string{}, DenyFiles: []string{filepath.Join(root, "main.tex")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	names = map[string]bool{}
	for _, file := range files {
		names[file.Path] = true
	}
	if !names["other/file.tex"] || names["main.tex"] {
		t.Fatalf("explicit sources: %v", names)
	}
	if _, _, err := Manifest(Options{Root: root, IgnoreFiles: []string{"absent"}}); err == nil {
		t.Fatal("explicit missing policy accepted")
	}
}

func TestNestedGitignoreReincludeWithoutRepository(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".gitignore"), "*.dat\n")
	mustWrite(t, filepath.Join(root, "sub/.gitignore"), "!keep.dat\n")
	mustWrite(t, filepath.Join(root, "sub/keep.dat"), "kept")
	mustWrite(t, filepath.Join(root, "sub/drop.dat"), "ignored")
	files, _, err := Manifest(Options{Root: root, RespectGitIgnore: true})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, file := range files {
		names[file.Path] = true
	}
	if !names["sub/keep.dat"] || names["sub/drop.dat"] {
		t.Fatalf("nested rules: %v", names)
	}
}
