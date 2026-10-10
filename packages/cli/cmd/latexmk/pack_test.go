package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func readPackZIP(t *testing.T, output string) map[string]string {
	t.Helper()
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	files := make(map[string]string)
	for _, file := range archive.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		closeErr := stream.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read ZIP: %v, %v", err, closeErr)
		}
		files[file.Name] = string(data)
	}
	return files
}

func TestPackDefaultIsOfflineAndHonorsSelectionPolicy(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, data := range map[string]string{
		".latexmk.json": `{"token":{"file":"missing-token"},"targets":{"paper":{"entry":"main.tex","engine":"pdflatex"}}}`,
		"main.tex":      `\documentclass{article}\input{chapter}\begin{document}ok\end{document}`,
		"chapter.tex":   "chapter", ".env": "SECRET=value", "secret.key": "private", "unused.txt": "unrelated",
	} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := captureCommandOutput(t, func() int { return run([]string{"latexmk", "pack", "--json"}) })
	if code != 0 {
		t.Fatalf("pack: %d %s %s", code, stdout, stderr)
	}
	var response struct {
		OK   bool
		Data packView
	}
	if err := json.Unmarshal(
		[]byte(stdout),
		&response,
	); err != nil || !response.OK ||
		response.Data.Engine != "pdflatex" {
		t.Fatalf("JSON: %s %v", stdout, err)
	}
	files := readPackZIP(t, "main-default.zip")
	if len(files) != 2 || files["chapter.tex"] != "chapter" {
		t.Fatal(files)
	}
	if _, err := os.Stat(".latexmk-cache"); !os.IsNotExist(err) {
		t.Fatalf("offline pack created project identity: %v", err)
	}
	code, stdout, stderr = captureCommandOutput(
		t,
		func() int { return run([]string{"latexmk", "pack", "--upload-mode", "all", "--json"}) },
	)
	if code != 0 {
		t.Fatalf("repeat pack: %d %s %s", code, stdout, stderr)
	}
	files = readPackZIP(t, "main-default.zip")
	if _, ok := files["main-default.zip"]; ok {
		t.Fatal("archive included itself")
	}
	for _, denied := range []string{".env", "secret.key", ".latexmk.json"} {
		if _, ok := files[denied]; ok {
			t.Fatalf("included %s", denied)
		}
	}
}

func TestPackArxivDryRunNeedsNoCredentialsOrOutput(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".latexmk.json", []byte(`{"token":{"file":"missing-token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureCommandOutput(
		t,
		func() int { return run([]string{"latexmk", "pack", "--mode=arxiv", "--dry-run", "--json", "main.tex"}) },
	)
	var response struct {
		OK   bool
		Data packView
	}
	if err := json.Unmarshal(
		[]byte(stdout),
		&response,
	); err != nil || code != 0 || !response.OK || !response.Data.RequiresBuild ||
		response.Data.Verified {
		t.Fatalf("preview: %d %s %s %v", code, stdout, stderr, err)
	}
	if _, err := os.Stat("main-arxiv.zip"); !os.IsNotExist(err) {
		t.Fatal("dry run wrote ZIP")
	}
}

func packArchive(t *testing.T, result protocol.CompileResult, artifacts map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{"stdout.log": "", "stderr.log": ""}
	write := func(name string, data []byte) {
		if err := writer.WriteHeader(
			&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("result.json", raw)
	for name, data := range entries {
		write(name, []byte(data))
	}
	for name, data := range artifacts {
		write("artifacts/"+name, []byte(data))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestPackArxivUsesOneSourceCaptureAndVerifiesFinalBundle(t *testing.T) {
	for _, failVerification := range []bool{false, true} {
		t.Run(fmt.Sprint(failVerification), func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			original := `\documentclass{article}\begin{document}original\end{document}`
			if err := os.WriteFile("main.tex", []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			var plans []protocol.UploadPlanRequest
			uploads := make(map[string]string)
			artifacts := map[string]string{
				"main.pdf": "compiled PDF",
				"main.bbl": "bibliography",
				"main.ind": "index",
				"main.gls": "glossary",
				"main.nls": "nomenclature",
				"main.aux": "temporary",
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/meta":
					_ = json.NewEncoder(
						w,
					).Encode(
						protocol.Metadata{
							ProtocolVersion: protocol.Version,
							Capabilities: protocol.Capabilities{
								QueuedJobs:         true,
								IncrementalUpload:  true,
								AuxiliaryRetention: true,
							},
						},
					)
				case r.URL.Path == "/v1/uploads/plans":
					var plan protocol.UploadPlanRequest
					if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
						t.Error(err)
					}
					plans = append(plans, plan)
					if err := os.WriteFile(
						filepath.Join(root, "main.tex"),
						[]byte("edited after capture"),
						0600,
					); err != nil {
						t.Error(err)
					}
					var missing []string
					for _, file := range plan.Files {
						missing = append(missing, file.SHA256)
					}
					_ = json.NewEncoder(
						w,
					).Encode(
						protocol.UploadPlan{UploadID: fmt.Sprintf("upl_%d", len(plans)), Missing: missing},
					)
				case strings.Contains(r.URL.Path, "/blobs/"):
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					uploads[filepath.Base(r.URL.Path)] = string(data)
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "/commit"):
					status := "succeeded"
					if failVerification && len(plans) == 2 {
						status = "failed"
					}
					_ = json.NewEncoder(w).Encode(protocol.Job{ID: fmt.Sprintf("job_%d", len(plans)), Status: status})
				case strings.HasSuffix(r.URL.Path, "/result"):
					result := protocol.CompileResult{
						ProtocolVersion: protocol.Version,
						RequestID:       fmt.Sprintf("job_%d", len(plans)),
						Entry:           "main.tex",
						Engine:          "pdflatex",
						Success:         !failVerification || len(plans) != 2,
						Error:           "fixture verification failed",
					}
					for name, data := range artifacts {
						hash := sha256.Sum256([]byte(data))
						result.Artifacts = append(
							result.Artifacts,
							protocol.Artifact{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])},
						)
					}
					_, _ = w.Write(packArchive(t, result, artifacts))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := os.WriteFile("paper.zip", []byte("previous package"), 0600); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := captureCommandOutput(t, func() int {
				return run(
					[]string{
						"latexmk",
						"pack",
						"--mode",
						"arxiv",
						"--engine",
						"pdflatex",
						"--server",
						server.URL,
						"--output",
						"paper.zip",
						"--json",
						"main.tex",
					},
				)
			})
			if len(plans) != 2 {
				t.Fatalf("expected source and bundle builds, got %d: %s %s", len(plans), stdout, stderr)
			}
			for _, plan := range plans {
				if !plan.Request.Force || plan.Request.Auxiliary.Local != "output" ||
					plan.Request.Auxiliary.Server != "none" {
					t.Fatal("pack build did not use cold, transfer-only policy")
				}
				for _, file := range plan.Files {
					if file.Path == "main.tex" && uploads[file.SHA256] != original {
						t.Fatal("pack used source edited after capture")
					}
				}
			}
			if failVerification {
				previous, err := os.ReadFile("paper.zip")
				if err != nil || string(previous) != "previous package" || code == 0 {
					t.Fatalf("failed verification replaced package: %s %v %d", previous, err, code)
				}
				return
			}
			if code != 0 {
				t.Fatalf("pack: %d %s %s", code, stdout, stderr)
			}
			files := readPackZIP(t, "paper.zip")
			if len(files) != 5 || files["main.tex"] != original || files["main.bbl"] != "bibliography" ||
				files["main.nls"] != "nomenclature" {
				t.Fatal(files)
			}
			if data, err := os.ReadFile("main.tex"); err != nil || string(data) != "edited after capture" {
				t.Fatal("pack changed working sources")
			}
		})
	}
}

func TestPackRejectsUnsupportedModesAndOperations(t *testing.T) {
	for _, args := range [][]string{{"--mode", "springer"}, {"--profile", "arxiv"}, {"--watch"}, {"--target", "all"}, {"--output", "paper.tar.gz"}, {"--server-cache", "reuse"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
				t.Fatal(err)
			}
			command := append([]string{"latexmk", "pack", "--json", "main.tex"}, args...)
			code, stdout, _ := captureCommandOutput(t, func() int { return run(command) })
			if code != 2 || !strings.Contains(stdout, `"invalid_arguments"`) {
				t.Fatalf("accepted unsupported operation: %d %s", code, stdout)
			}
		})
	}
}
