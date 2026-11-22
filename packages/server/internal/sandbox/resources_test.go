package sandbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/server/internal/archive"
	"github.com/billstark001/latexmk/packages/server/internal/resultarchive"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func resourceWorkerInput(t *testing.T, tamper bool) []byte {
	t.Helper()
	staging := t.TempDir()
	if _, err := projectarchive.ExtractTarGz(bytes.NewReader(workerInput(t, "source", "", true)), staging,
		projectarchive.Limits{MaxFiles: 100, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	var request workerRequest
	raw, err := os.ReadFile(filepath.Join(staging, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	request.Request.EnvironmentDigest = strings.Repeat("a", 64)
	request.MaxResourceBytes, request.MaxResourceFiles = 4096, 10
	for _, group := range []struct{ kind, name, content string }{
		{"project", "fonts/body.otf", "private font"},
		{"texmf", "tex/latex/example/example.sty", "private macro"},
	} {
		name := "resources/" + group.kind + "/" + group.name
		if err := os.MkdirAll(filepath.Dir(filepath.Join(staging, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staging, name), []byte(group.content), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := describeFile(staging, name, 4096)
		if err != nil {
			t.Fatal(err)
		}
		manifest := resources.File{Path: group.name, SHA256: file.SHA256, Size: file.Size}
		if group.kind == "project" {
			request.Resources.Files = append(request.Resources.Files, manifest)
		} else {
			request.Resources.TexmfFiles = append(request.Resources.TexmfFiles, manifest)
		}
	}
	if tamper {
		request.Resources.Files[0].SHA256 = strings.Repeat("b", 64)
	}
	raw, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "request.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	members, err := collectState(staging, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	if err := writeArchive(&result, members, gzip.BestSpeed); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestWorkerResourceTransportIsVerifiedAndExcludedFromCheckpoint(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
set -eu
test "$(cat fonts/body.otf)" = 'private font'
test "$(cat "$TEXMFHOME/tex/latex/example/example.sty")" = 'private macro'
printf 'PDF' > .latexmk-build/main.pdf
printf 'aux' > .latexmk-build/main.aux
printf 'PWD %s\nINPUT main.tex\nINPUT fonts/body.otf\nINPUT .latexmk-home/texmf/tex/latex/example/example.sty\nOUTPUT .latexmk-build/main.pdf\nOUTPUT .latexmk-build/main.aux\n' "$PWD" > .latexmk-build/main.fls
`
	if err := os.WriteFile(filepath.Join(bin, "latexmk"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tamper := range []bool{false, true} {
		root := t.TempDir()
		t.Cleanup(func() {
			_ = filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
				if err == nil && info.IsDir() {
					return os.Chmod(name, 0700)
				}
				return err
			})
		})
		var response bytes.Buffer
		err := runWorker(
			context.Background(),
			bytes.NewReader(resourceWorkerInput(t, tamper)),
			&response,
			root,
			1<<20,
			100,
		)
		if tamper {
			if err == nil {
				t.Fatal("tampered resource input was accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		outer := t.TempDir()
		if _, err := projectarchive.ExtractTarGz(
			&response,
			outer,
			projectarchive.Limits{MaxFiles: 2, MaxBytes: 1 << 20},
		); err != nil {
			t.Fatal(err)
		}
		archive, err := os.Open(filepath.Join(outer, "result.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := resultarchive.Decode(
			archive,
			t.TempDir(),
			resultarchive.Limits{MaxFiles: 100, MaxArtifacts: 8192, MaxLogs: 8192},
		)
		_ = archive.Close()
		if err != nil || !result.Result.Success || result.Result.EnvironmentDigest != strings.Repeat("a", 64) {
			t.Fatalf("resource compile result: %+v %v", result.Result, err)
		}
		if !reflect.DeepEqual(result.Result.InputFiles, []string{"main.tex"}) {
			t.Fatalf("managed resources escaped into project dependency history: %v", result.Result.InputFiles)
		}
		checkpoint, err := os.Open(filepath.Join(outer, "checkpoint.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		state := t.TempDir()
		_, err = projectarchive.ExtractTarGz(checkpoint, state, projectarchive.Limits{MaxFiles: 100, MaxBytes: 8192})
		_ = checkpoint.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(state, "fonts")); !os.IsNotExist(err) {
			t.Fatal("resource files were checkpointed", err)
		}
	}
}
