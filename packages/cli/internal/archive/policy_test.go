package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestNestedGitignoreAnchorsAndParentPruning(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		".gitignore":          "*.dat\npruned/\n",
		"sub/.gitignore":      "!keep.dat\n/root.dat\n",
		"sub/deep/.gitignore": "!*.dat\n",
		"pruned/.gitignore":   "!allow.dat\n",
		"main.tex":            "main", "top.dat": "excluded", "sub/keep.dat": "included",
		"sub/root.dat": "excluded", "sub/deep/root.dat": "included", "sub/deep/deep.dat": "included", "pruned/allow.dat": "excluded",
	} {
		mustWrite(t, filepath.Join(root, name), body)
	}
	files, _, err := Manifest(Options{Root: root, RespectGitIgnore: true, Exclude: []string{".gitignore"}})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, file := range files {
		names[file.Path] = true
	}
	for name, want := range map[string]bool{"main.tex": true, "top.dat": false, "sub/keep.dat": true, "sub/root.dat": false, "sub/deep/root.dat": true, "sub/deep/deep.dat": true, "pruned/allow.dat": false} {
		if names[name] != want {
			t.Errorf("%s included=%v, want=%v", name, names[name], want)
		}
	}
}

func TestNestedGitignoreMatchesGitWithBlankLinesAndLiteralDirectory(t *testing.T) {
	for _, base := range []string{"sub", "sub[1]"} {
		t.Run(base, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range map[string]string{
				".gitignore": "\r\n   \r\n# comment\r\n/\r\n!\r\n*.tmp  \r\n/root.dat\r\n\\#literal\r\n\\!literal\r\n",
				"main.tex":   "main", "deep/body.tex": "body",
				"deep/drop.tmp": "ignored", "root.dat": "ignored",
				"deep/root.dat": "kept", "#literal": "ignored", "!literal": "ignored",
			} {
				mustWrite(t, filepath.Join(root, base, name), body)
			}
			opts := Options{Root: root, RespectGitIgnore: true, Exclude: []string{".gitignore"}}
			withoutGit, _, err := Manifest(opts)
			if err != nil {
				t.Fatal(err)
			}
			mustRun(t, root, "git", "init", "--quiet")
			withGit, _, err := Manifest(opts)
			if err != nil {
				t.Fatal(err)
			}
			names := func(files []File) string {
				var paths []string
				for _, file := range files {
					paths = append(paths, file.Path)
				}
				return strings.Join(paths, "\n")
			}
			if got, want := names(withoutGit), names(withGit); got != want {
				t.Fatalf("fallback selection differs from Git:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func BenchmarkNestedPolicyManyDirectories(b *testing.B) {
	root := b.TempDir()
	var rules strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&rules, "unused-%d.tmp\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(rules.String()), 0600); err != nil {
		b.Fatal(err)
	}
	var paths []string
	for i := 0; i < 100; i++ {
		paths = append(paths, fmt.Sprintf("directory-%d/main.tex", i))
	}
	for b.Loop() {
		policy, err := newPolicy(Options{Root: root, RespectGitIgnore: true})
		if err != nil {
			b.Fatal(err)
		}
		for _, name := range paths {
			if _, _, err := policy.excluded(name, false); err != nil {
				b.Fatal(err)
			}
		}
	}
}
