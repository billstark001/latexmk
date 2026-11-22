package resources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	model "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func uploadDeduplicatedFiles(
	t *testing.T,
	manager *Manager,
	request model.ResolveRequest,
	contents map[string]string,
) model.Environment {
	t.Helper()
	environment, err := manager.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	for digest, content := range contents {
		if err := manager.PutBlob(
			context.Background(),
			"alice",
			environment.Digest,
			digest,
			strings.NewReader(content),
		); err != nil {
			t.Fatal(err)
		}
	}
	environment, err = manager.Prepare(context.Background(), "alice", environment.Digest)
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func TestDuplicateResourceFilesPreserveProvenanceAndFontKind(t *testing.T) {
	fontBytes, licenseBytes := "an uploaded font", "font license text"
	font := reviewFile("Paper.otf", fontBytes)
	license := reviewFile("LICENSE.txt", licenseBytes)
	for _, fontFirst := range []bool{true, false} {
		name := "font-sorts-last"
		if fontFirst {
			name = "font-sorts-first"
		}
		t.Run(name, func(t *testing.T) {
			manager := testManager(t)
			first := model.Resource{
				ID:             "a-resource",
				Type:           "files",
				Kind:           "data",
				Mount:          "vendor/fonts",
				Files:          []model.File{font, license},
				LicenseFiles:   []string{license.Path},
				Redistribution: "allow",
			}
			second := first
			second.ID = "z-resource"
			if fontFirst {
				first.Kind = "font"
			} else {
				second.Kind = "font"
			}
			// Reverse the submitted order as well. The result must follow logical
			// destinations and kinds, independently of declaration ordering.
			request := model.ResolveRequest{Resources: []model.Resource{second, first}}
			environment := uploadDeduplicatedFiles(
				t,
				manager,
				request,
				map[string]string{font.SHA256: fontBytes, license.SHA256: licenseBytes},
			)
			if len(environment.Resources) != 2 {
				t.Fatalf("resolution collapsed resource provenance: %#v", environment.Resources)
			}
			lease, err := manager.Pin("alice", environment.Digest)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if len(lease.Environment().Resources) != 2 {
				t.Fatal("pinning collapsed the declared resources")
			}
			files := lease.Files()
			if len(files) != 2 {
				t.Fatalf("duplicate logical files reached the runner: %#v", files)
			}
			seen := make(map[string]bool)
			for _, file := range files {
				if seen[file.Path] {
					t.Fatalf("logical path %q appears more than once", file.Path)
				}
				seen[file.Path] = true
				if file.Kind != "font" {
					t.Fatalf("font activation was lost when merging %q: %#v", file.Path, file)
				}
			}
			for _, expected := range []string{"vendor/fonts/Paper.otf", "vendor/fonts/LICENSE.txt"} {
				if !seen[expected] {
					t.Fatalf("missing logical destination %q", expected)
				}
			}
		})
	}
}

func TestDuplicateTEXMFResourcesMaterializeOneTreeFile(t *testing.T) {
	manager := testManager(t)
	content := "\\ProvidesPackage{shared}"
	file := reviewFile("tex/latex/shared/shared.sty", content)
	first := model.Resource{
		ID:             "first",
		Type:           "files",
		Kind:           "texmf",
		Mount:          ".",
		Files:          []model.File{file},
		Redistribution: "allow",
	}
	second := first
	second.ID = "second"
	environment := uploadDeduplicatedFiles(
		t,
		manager,
		model.ResolveRequest{Resources: []model.Resource{first, second}},
		map[string]string{file.SHA256: content},
	)
	if len(environment.Resources) != 2 {
		t.Fatal("TEXMF deduplication discarded the source declarations")
	}
	lease, err := manager.Pin("alice", environment.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if len(lease.Files()) != 0 || lease.Tree() == "" {
		t.Fatalf("TEXMF inputs were not confined to their prepared tree: %#v", lease.Files())
	}
	files, err := inventoryFiles(lease.Tree(), manager.cfg.MaxResourceBytes, manager.cfg.MaxResourceFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != file {
		t.Fatalf("prepared tree contains duplicate or changed bytes: %#v", files)
	}
}

func TestDuplicateResourceDestinationRejectsDifferentBytes(t *testing.T) {
	first := model.Resource{
		ID:    "first",
		Type:  "files",
		Kind:  "data",
		Mount: "vendor",
		Files: []model.File{reviewFile("shared.dat", "original")},
	}
	second := first
	second.ID = "second"
	second.Files = []model.File{reviewFile("shared.dat", "modified")}
	request := model.ResolveRequest{Resources: []model.Resource{first, second}}
	if _, err := model.NormalizeRequest(request); err == nil {
		t.Fatal("normalization accepted two versions of the same destination")
	}
	if _, err := testManager(t).Resolve(context.Background(), "alice", request); err == nil {
		t.Fatal("resolution accepted two versions of the same destination")
	}
}

func TestMergeTEXMFFileReusesExistingPackageBytesAndRejectsConflicts(t *testing.T) {
	for _, test := range []struct {
		name, resourceBytes string
		wantErr             bool
	}{
		{"same-installed-bytes", "original", false},
		{"different-installed-bytes", "modified", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			name := "tex/latex/package/package.sty"
			installed := filepath.Join(directory, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(installed), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(installed, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			file := reviewFile(name, test.resourceBytes)
			source := filepath.Join(t.TempDir(), file.SHA256)
			if err := os.WriteFile(source, []byte(test.resourceBytes), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := safefs.Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			err = mergeTEXMFFile(root, name, file, source)
			if (err != nil) != test.wantErr {
				t.Fatalf("merge result: %v", err)
			}
			contents, err := os.ReadFile(installed)
			if err != nil || string(contents) != "original" {
				t.Fatalf("merge replaced existing package bytes: %q %v", contents, err)
			}
			files, err := inventoryFiles(directory, 4096, 20)
			if err != nil || len(files) != 1 {
				t.Fatalf("merge left an extra file: %#v %v", files, err)
			}
		})
	}
}
