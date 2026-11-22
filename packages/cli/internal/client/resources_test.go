package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	shared "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestResourcesSatisfyDiscoveryWithoutEnteringSourceManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "private/fonts"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"main.tex": "\\documentclass{article}\n\\usepackage{fontspec}\n\\setmainfont{font.otf}[Path=fonts/]\n\\begin{document}x\\end{document}", "private/fonts/font.otf": "font", "private/fonts/unused.otf": "never publish"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	captured, err := cliresources.Capture(
		context.Background(),
		root,
		map[string]shared.Spec{
			"font": {Type: "files", Path: "private/fonts", Mount: "fonts", Include: []string{"font.otf"}},
		},
		[]string{"font"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	for _, mode := range []string{"auto", "all"} {
		t.Run(mode, func(t *testing.T) {
			c := &Client{
				ProjectRoot:   root,
				UploadMode:    mode,
				DenyFiles:     captured.DenyPaths,
				ResourceFiles: captured.MountFiles(),
			}
			selected, err := c.Selection("main.tex", "xelatex", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !selected.Resolved || len(selected.Files) != 1 || selected.Files[0].Path != "main.tex" {
				t.Fatalf("resource leaked or reference unresolved: %#v", selected)
			}
		})
	}
}

func TestEnvironmentAPIUploadsMissingBlobsBeforePreparation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "font.otf"), []byte("font"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := cliresources.Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"font": {Type: "files", Path: "font.otf", Mount: "fonts"}},
		[]string{"font"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	environment := shared.Environment{
		Baseline: shared.Baseline{
			ID:      "slim-v1",
			Digest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Engines: []string{"xelatex"},
		},
		Resources: captured.Resources,
	}
	environment.Digest, err = shared.EnvironmentDigest(environment)
	if err != nil {
		t.Fatal(err)
	}
	environment.Missing = []string{captured.Resources[0].Files[0].SHA256}
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			if r.URL.Path == "/v1/environments/resolve" {
				if err := json.NewEncoder(w).Encode(environment); err != nil {
					t.Error(err)
				}
			} else {
				prepared := environment
				prepared.Ready = true
				prepared.Missing = nil
				if err := json.NewEncoder(w).Encode(prepared); err != nil {
					t.Error(err)
				}
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := c.ResolveEnvironment(context.Background(), shared.ResolveRequest{Resources: captured.Resources})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareEnvironment(context.Background(), resolved, captured.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Ready {
		t.Fatal("not ready")
	}
	want := []string{
		"POST /v1/environments/resolve",
		"PUT /v1/environments/" + environment.Digest + "/blobs/" + environment.Missing[0],
		"POST /v1/environments/" + environment.Digest + "/prepare",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%#v", calls)
	}
}

func TestBaselineFontFilesAreFallbacksAndNeverUploaded(t *testing.T) {
	root := t.TempDir()
	source := `\documentclass{article}\usepackage{fontspec}\setmainfont{baseline.otf}\begin{document}x\end{document}`
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{ProjectRoot: root, UploadMode: "auto"}
	c.SetBaseline(shared.Baseline{ID: "slim-v1", Fonts: []shared.Font{{File: "/usr/share/fonts/baseline.otf"}}})
	selection, err := c.Selection("main.tex", "xelatex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Resolved || len(selection.Files) != 1 {
		t.Fatalf("baseline font not satisfied or uploaded: %#v", selection)
	}
	if err := os.WriteFile(filepath.Join(root, "baseline.otf"), []byte("project font"), 0600); err != nil {
		t.Fatal(err)
	}
	selection, err = c.Selection("main.tex", "xelatex", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Resolved || len(selection.Files) != 2 {
		t.Fatalf("project font lost to baseline: %#v", selection)
	}
}

func TestPrepareEnvironmentValidatesAllUploadsBeforeSendingBytes(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{"selected.dat": "selected bytes", "private.dat": "unrelated private bytes"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	captured, err := cliresources.Capture(
		context.Background(),
		root,
		map[string]shared.Spec{
			"selected": {Type: "files", Path: "selected.dat"},
			"private":  {Type: "files", Path: "private.dat"},
		},
		[]string{"selected", "private"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	resourcesByID := make(map[string]shared.Resource)
	for _, resource := range captured.Resources {
		resourcesByID[resource.ID] = resource
	}
	selected, private := resourcesByID["selected"], resourcesByID["private"]
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "no request expected", http.StatusInternalServerError)
	}))
	defer server.Close()
	c, err := New(server.URL, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		resources []shared.Resource
		blobs     map[string]projectarchive.File
	}{
		{name: "undeclared digest", resources: []shared.Resource{selected}, blobs: captured.Blobs},
		{name: "unavailable declared file", resources: []shared.Resource{selected, private}, blobs: map[string]projectarchive.File{selected.Files[0].SHA256: captured.Blobs[selected.Files[0].SHA256]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := shared.Environment{
				Baseline:  shared.Baseline{ID: "slim-v1", Digest: strings.Repeat("a", 64)},
				Resources: test.resources,
				Missing:   []string{selected.Files[0].SHA256, private.Files[0].SHA256},
			}
			environment.Digest, err = shared.EnvironmentDigest(environment)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.PrepareEnvironment(context.Background(), environment, test.blobs); err == nil {
				t.Fatal("invalid pending upload accepted")
			}
			if requests != 0 {
				t.Fatal("a partial upload escaped before pending-file validation completed")
			}
		})
	}
}
