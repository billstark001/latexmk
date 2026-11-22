package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func resourceFixture(t *testing.T, content string) File {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	return File{
		Workspace:    root,
		RelativePath: "input",
		Size:         int64(len(content)),
		SHA256:       hex.EncodeToString(digest[:]),
	}
}

func TestResourceMaterializationConflictsTamperingAndPrivateCopies(t *testing.T) {
	font := resourceFixture(t, "font bytes")
	macro := resourceFixture(t, "macro bytes")
	input := ResourceInput{Files: []ResourceFile{{Path: "fonts/body.otf", File: font}},
		TexmfFiles: []ResourceFile{{Path: "tex/latex/example/example.sty", File: macro}}}
	workspace := t.TempDir()
	if err := MaterializeResources(workspace, input); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fonts/body.otf", ".latexmk-home/texmf/tex/latex/example/example.sty"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(workspace, "fonts/body.otf"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "fonts/body.otf"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeResources(workspace, input); err == nil {
		t.Fatal("resource overwrite conflict was ignored")
	}
	original, err := os.ReadFile(filepath.Join(font.Workspace, font.RelativePath))
	if err != nil || string(original) != "font bytes" {
		t.Fatal("compiler copy changed the shared resource", err)
	}
	if err := os.WriteFile(filepath.Join(font.Workspace, font.RelativePath), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeResources(t.TempDir(), input); err == nil {
		t.Fatal("resource content was not checked at materialization")
	}
}

func TestResourceTreeRejectsLinksAndExpandedLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "macro.sty"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DescribeResourceTree(root, 1, 3); err == nil {
		t.Fatal("expanded byte limit ignored")
	}
	if err := os.Symlink("macro.sty", filepath.Join(root, "alias.sty")); err != nil {
		t.Skip(err)
	}
	if _, err := DescribeResourceTree(root, 10, 1024); err == nil {
		t.Fatal("resource tree followed a symlink")
	}
}

func TestResourceFontEnvironmentEscapesPathsAndKeepsPrivateState(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".latexmk-home"), 0700); err != nil {
		t.Fatal(err)
	}
	values, err := resourceFontEnvironment(root, ResourceInput{FontLookup: "names", FontDirs: []string{"fonts/a&b"}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, ".latexmk-home/fonts.conf"))
	if err != nil || !strings.Contains(string(config), "a&amp;b") || len(values) != 3 {
		t.Fatalf("font config = %s, %v, %v", config, values, err)
	}
	values, err = resourceFontEnvironment(root, ResourceInput{FontLookup: "files", FontDirs: []string{"fonts"}})
	if err != nil || len(values) != 0 {
		t.Fatal("file-only baseline unexpectedly registers named fonts", values, err)
	}
}

func TestManagedResourcesCannotBeReturnedAsArtifactsOrConsumeOutputQuota(t *testing.T) {
	for _, buildDir := range []string{"", ".latexmk-build"} {
		t.Run(map[bool]string{false: "native", true: "isolated"}[buildDir != ""], func(t *testing.T) {
			root := t.TempDir()
			prefix := ""
			if buildDir != "" {
				prefix = buildDir + "/"
			}
			files := map[string]string{
				prefix + "main.pdf":                "output",
				prefix + "main.fls":                "OUTPUT " + prefix + "main.pdf\nOUTPUT private/main.pdf\nOUTPUT .latexmk-home/texmf/doc/main.pdf\n",
				"private/main.pdf":                 strings.Repeat("private", 100),
				"private/main.fls":                 "OUTPUT unrelated.pdf\n",
				"unrelated.pdf":                    strings.Repeat("unrelated", 100),
				".latexmk-home/texmf/doc/main.pdf": strings.Repeat("package documentation", 100),
			}
			for name, content := range files {
				full := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			managed := map[string]bool{"private/main.pdf": true, "private/main.fls": true}
			// Only actual outputs and their recorder count, not resources or TEXMF docs.
			limit := int64(len(files[prefix+"main.pdf"]) + len(files[prefix+"main.fls"]))
			artifacts, err := collectArtifacts(
				root,
				protocol.CompileRequest{Entry: "main.tex"},
				limit,
				buildDir,
				managed,
			)
			if err != nil {
				t.Fatal("resource file consumed output quota", err)
			}
			for _, artifact := range artifacts {
				if artifact.RelativePath != "main.pdf" && artifact.RelativePath != "main.fls" {
					t.Fatal("managed resource or its recorder escaped as an artifact", artifact.RelativePath)
				}
				if artifact.Workspace != filepath.Join(root, buildDir) {
					t.Fatal("artifact was not rooted at its actual output directory", artifact.Workspace)
				}
				file, err := artifact.Open()
				if err != nil {
					t.Fatal("normalized artifact cannot be opened", err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if len(artifacts) != 2 {
				t.Fatalf("missing real outputs: %#v", artifacts)
			}
		})
	}
}

func TestRecorderCleanupPreservesManagedInputBytes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"main.fls", "private/input.fls", ".latexmk-home/texmf/data.fls"} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("recorder"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeStaleRecorderFiles(root, map[string]bool{"private/input.fls": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "main.fls")); !os.IsNotExist(err) {
		t.Fatal("stale generated recorder survived", err)
	}
	for _, name := range []string{"private/input.fls", ".latexmk-home/texmf/data.fls"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatal("resource input was mutated by generated-file cleanup", err)
		}
	}
}
