package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

func TestUnpackResponseRejectsArtifactTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	result := []byte(`{"protocolVersion":1,"requestId":"req_test","success":true,"exitCode":0}`)
	if err := tw.WriteHeader(&tar.Header{Name: "result.json", Mode: 0o644, Size: int64(len(result)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(result); err != nil {
		t.Fatal(err)
	}
	payload := []byte("bad")
	if err := tw.WriteHeader(&tar.Header{Name: "artifacts/../../escape", Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var output CompileOutput
	if err := unpackResponse(bytes.NewReader(buf.Bytes()), t.TempDir(), &output); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestUnpackResponseDoesNotInstallHashMismatch(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	result := []byte(`{"protocolVersion":1,"requestId":"req_test","success":true,"exitCode":0,"artifacts":[{"path":"main.pdf","size":3,"sha256":"0000000000000000000000000000000000000000000000000000000000000000"}]}`)
	if err := tw.WriteHeader(&tar.Header{Name: "result.json", Mode: 0o644, Size: int64(len(result)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(result)
	payload := []byte("pdf")
	if err := tw.WriteHeader(&tar.Header{Name: "artifacts/main.pdf", Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	root := t.TempDir()
	var output CompileOutput
	if err := unpackResponse(bytes.NewReader(buf.Bytes()), root, &output); err == nil {
		t.Fatal("expected hash mismatch")
	}
	if _, err := os.Stat(filepath.Join(root, "main.pdf")); !os.IsNotExist(err) {
		t.Fatalf("mismatched artifact was installed: %v", err)
	}
}

func TestNewRejectsUnsafeServerURLs(t *testing.T) {
	for _, raw := range []string{
		"localhost:8080",
		"ftp://example.test",
		"https://user:pass@example.test",
		"https://example.test?token=secret",
		"https://example.test/#fragment",
	} {
		if _, err := New(raw, "", 0, false); err == nil {
			t.Fatalf("expected URL %q to be rejected", raw)
		}
	}
	if _, err := New("https://example.test/api", "", 0, false); err != nil {
		t.Fatalf("expected valid URL: %v", err)
	}
}

func TestJobMethodsUseStablePathsAndOrdering(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []protocol.Job{
				{ID: "job_old", ProjectID: "project", Status: "succeeded", CreatedAt: older},
				{ID: "job_b", ProjectID: "project", Status: "queued", CreatedAt: newer},
				{ID: "job_a", ProjectID: "project", Status: "queued", CreatedAt: newer},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_a":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_a", ProjectID: "project", Status: "queued", CreatedAt: newer})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/jobs/job_a":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_a", ProjectID: "project", Status: "cancelled", CreatedAt: newer})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := c.ListJobs(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || jobs[0].ID != "job_a" || jobs[1].ID != "job_b" || jobs[2].ID != "job_old" {
		t.Fatalf("jobs are not stably sorted: %#v", jobs)
	}
	job, err := c.GetJob(context.Background(), "job_a")
	if err != nil || job.ID != "job_a" {
		t.Fatalf("get job = %#v, %v", job, err)
	}
	job, err = c.CancelJob(context.Background(), "job_a")
	if err != nil || job.Status != "cancelled" {
		t.Fatalf("cancel job = %#v, %v", job, err)
	}
	want := []string{"GET /v1/jobs?limit=3", "GET /v1/jobs/job_a", "DELETE /v1/jobs/job_a"}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestHTTPErrorPreservesStatusWithoutRequestCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"job not found"}`))
	}))
	defer server.Close()
	c, err := New(server.URL, "secret-token", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetJob(context.Background(), "job_missing")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatal("HTTP error exposed the bearer token")
	}
}

func TestUnpackResponseRejectsProtocolBeforeWriting(t *testing.T) {
	payload := []byte("pdf")
	result := []byte(`{"protocolVersion":99,"requestId":"req_test","success":true,"exitCode":0,"artifacts":[{"path":"main.pdf","size":3,"sha256":"c35b21d6ca39aa7cc3b79a705d989f1a6e88b99ab43988d74048799e3db926a3"}]}`)
	archive := buildResultArchive(t, []tarEntry{
		{name: "result.json", payload: result},
		{name: "artifacts/main.pdf", payload: payload},
	})
	root := t.TempDir()
	var output CompileOutput
	if err := unpackResponse(bytes.NewReader(archive), root, &output); err == nil {
		t.Fatal("expected protocol rejection")
	}
	if _, err := os.Stat(filepath.Join(root, "main.pdf")); !os.IsNotExist(err) {
		t.Fatalf("artifact was installed before protocol validation: %v", err)
	}
}

func TestCompileUsesQueuedIncrementalProtocol(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated-secret.txt"), []byte("do not upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var planned protocol.UploadPlanRequest
	var uploaded []byte
	resultArchive := buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":2,"requestId":"job_test","success":true,"exitCode":0,"inputFiles":["main.tex"]}`)}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 2, Capabilities: protocol.Capabilities{IncrementalUpload: true, QueuedJobs: true, DependencyInputs: true}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/uploads/plans":
			if err := json.NewDecoder(r.Body).Decode(&planned); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_test", Missing: []string{planned.Files[0].SHA256}, ExpiresAt: time.Now().Add(time.Minute)})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/uploads/upl_test/blobs/"+planned.Files[0].SHA256:
			uploaded, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/uploads/upl_test/commit":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_test", Status: "queued"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_test":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_test", Status: "succeeded"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_test/result":
			w.Header().Set("Content-Type", "application/vnd.latexmk.result+tar.gz")
			_, _ = w.Write(resultArchive)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	client.ProjectRoot = root
	output, err := client.Compile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !output.Result.Success || string(uploaded) != "hello" || len(planned.Files) != 1 {
		t.Fatalf("queued compile result=%#v upload=%q plan=%#v", output.Result, uploaded, planned)
	}
	if !planned.Request.RecordInputs {
		t.Fatal("client did not negotiate recorder INPUT results")
	}
	if _, err := os.Stat(filepath.Join(root, ".latexmk-cache", "dependencies.json")); err != nil {
		t.Fatalf("dependency cache was not saved: %v", err)
	}
}

func TestProjectManifestUsesCachedInputsForDynamicReferences(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte(`\input{\chapterfile}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "chapter.tex"), []byte("chapter"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dependency.SaveCachedInputs(root, "main.tex", "xelatex", []string{"main.tex", "chapter.tex"}); err != nil {
		t.Fatal(err)
	}
	c, err := New("http://127.0.0.1:1", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	files, warnings, err := c.projectManifest("main.tex", "xelatex")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "chapter.tex" || files[1].Path != "main.tex" {
		t.Fatalf("cached manifest = %#v", files)
	}
	if len(warnings) != 1 {
		t.Fatalf("cached manifest warnings = %#v", warnings)
	}
}

func TestProjectManifestUsesExplicitManifestWithoutHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte(`\input{\chapterfile}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "chapter.tex"), []byte("chapter"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated-secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".latexmk-files"), []byte("chapter.tex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New("http://127.0.0.1:1", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	c.Exclude = []string{".latexmk-files"}
	c.ManifestFile = ".latexmk-files"
	files, warnings, err := c.projectManifest("main.tex", "xelatex")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "chapter.tex" || files[1].Path != "main.tex" {
		t.Fatalf("explicit manifest files = %#v", files)
	}
	if len(warnings) != 1 {
		t.Fatalf("explicit manifest warnings = %#v", warnings)
	}
}

func TestProjectManifestExplicitModesCanBypassBrokenCache(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(root, ".latexmk-cache")
	if err := os.Mkdir(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "dependencies.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New("http://127.0.0.1:1", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	c.Exclude = []string{".latexmk-cache"}
	c.UploadMode = "all"
	files, _, err := c.projectManifest("main.tex", "xelatex")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatalf("all-mode manifest = %#v", files)
	}
	c.UploadMode = "manifest"
	files, _, err = c.projectManifest("main.tex", "xelatex")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatalf("manifest-mode files = %#v", files)
	}
}

func TestProjectManifestNeverUploadsConfiguredManifestFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "policy.list"), []byte("main.tex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New("http://127.0.0.1:1", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	c.UploadMode = "all"
	c.ManifestFile = "policy.list"
	files, _, err := c.projectManifest("main.tex", "xelatex")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatalf("all-mode files = %#v", files)
	}
}

func TestCompileLegacyArchiveExcludesUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated-secret.txt"), []byte("do not upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var uploaded []string
	var receivedRequest protocol.CompileRequest
	resultArchive := buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":1,"requestId":"req_test","success":true,"exitCode":0}`)}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 1})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/compile":
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if part.FormName() == "request" {
					if err := json.NewDecoder(part).Decode(&receivedRequest); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_ = part.Close()
					continue
				}
				if part.FormName() != "project" {
					_, _ = io.Copy(io.Discard, part)
					_ = part.Close()
					continue
				}
				gz, err := gzip.NewReader(part)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				tarReader := tar.NewReader(gz)
				for {
					header, err := tarReader.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					uploaded = append(uploaded, header.Name)
				}
				_ = gz.Close()
				_ = part.Close()
			}
			w.Header().Set("Content-Type", "application/vnd.latexmk.result+tar.gz")
			_, _ = w.Write(resultArchive)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	output, err := c.Compile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !output.Result.Success || len(uploaded) != 1 || uploaded[0] != "main.tex" {
		t.Fatalf("legacy compile result=%#v uploaded=%#v", output.Result, uploaded)
	}
	if receivedRequest.RecordInputs {
		t.Fatal("client sent recordInputs to a server that did not advertise it")
	}
	if receivedRequest.DetectMissingFiles {
		t.Fatal("client sent detectMissingFiles to a server that did not advertise it")
	}
}

func TestCompileRetriesMissingFilesWithNewAllowedManifest(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"main.tex":             "main",
		"needed.tex":           "needed",
		"unrelated-secret.txt": "secret",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	responses := [][]byte{
		buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":1,"requestId":"req_first","success":false,"exitCode":12,"needsFiles":["needed.tex"]}`)}}),
		buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":1,"requestId":"req_second","success":true,"exitCode":0}`)}}),
	}
	var uploads [][]string
	var requests []protocol.CompileRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 1, Capabilities: protocol.Capabilities{NeedsFiles: true}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/compile":
			req, files, err := readCompileMultipart(r)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			requests = append(requests, req)
			uploads = append(uploads, files)
			if len(uploads) > len(responses) {
				t.Errorf("unexpected extra compile request")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.latexmk.result+tar.gz")
			_, _ = w.Write(responses[len(uploads)-1])
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	output, err := c.Compile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !output.Result.Success || len(uploads) != 2 {
		t.Fatalf("compile result=%#v uploads=%#v", output.Result, uploads)
	}
	if got := strings.Join(uploads[0], ","); got != "main.tex" {
		t.Fatalf("first snapshot = %q", got)
	}
	if got := strings.Join(uploads[1], ","); got != "main.tex,needed.tex" {
		t.Fatalf("retry snapshot = %q", got)
	}
	if len(requests) != 2 || !requests[0].DetectMissingFiles || !requests[1].DetectMissingFiles {
		t.Fatalf("missing-file capability was not negotiated: %#v", requests)
	}
	if got := strings.Join(output.Warnings, "\n"); !strings.Contains(got, "new immutable snapshot") {
		t.Fatalf("retry warning = %q", got)
	}
}

func TestCompileRefusesMissingFileOutsideLocalPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=value"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":1,"requestId":"req_failed","success":false,"exitCode":12,"needsFiles":[".env"]}`)}})
	compileCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 1, Capabilities: protocol.Capabilities{NeedsFiles: true}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/compile":
			compileCalls++
			w.Header().Set("Content-Type", "application/vnd.latexmk.result+tar.gz")
			_, _ = w.Write(response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	c.Exclude = []string{".env"}
	output, err := c.Compile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if output.Result.Success || compileCalls != 1 {
		t.Fatalf("result=%#v compile calls=%d", output.Result, compileCalls)
	}
	if got := strings.Join(output.Warnings, "\n"); !strings.Contains(got, "ignored, or denied") {
		t.Fatalf("policy refusal warning = %q", got)
	}
}

func TestCompileManifestModeDoesNotNegotiateMissingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":1,"requestId":"req_failed","success":false,"exitCode":12,"needsFiles":["extra.tex"]}`)}})
	compileCalls := 0
	var received protocol.CompileRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 1, Capabilities: protocol.Capabilities{NeedsFiles: true}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/compile":
			compileCalls++
			var err error
			received, _, err = readCompileMultipart(r)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.latexmk.result+tar.gz")
			_, _ = w.Write(response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	c.UploadMode = "manifest"
	output, err := c.Compile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if output.Result.Success || compileCalls != 1 || received.DetectMissingFiles {
		t.Fatalf("manifest result=%#v calls=%d request=%#v", output.Result, compileCalls, received)
	}
}

func TestCompileRejectsIncompleteDependenciesBeforeNetwork(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte(`\input{\dynamicfile}`), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	c, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	_, err = c.Compile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex"}, root)
	if err == nil {
		t.Fatal("expected dynamic dependency to block compilation")
	}
	if requests != 0 {
		t.Fatalf("client contacted server %d times before dependency validation", requests)
	}
}

func TestUnpackResponseRejectsUnexpectedEntry(t *testing.T) {
	result := []byte(`{"protocolVersion":1,"requestId":"req_test","success":true,"exitCode":0}`)
	archive := buildResultArchive(t, []tarEntry{
		{name: "result.json", payload: result},
		{name: "surprise.txt", payload: []byte("unexpected")},
	})
	var output CompileOutput
	if err := unpackResponse(bytes.NewReader(archive), t.TempDir(), &output); err == nil {
		t.Fatal("expected unexpected entry rejection")
	}
}

func TestWriteArtifactRejectsSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	payload := []byte("pdf")
	digest := sha256.Sum256(payload)
	err := writeArtifact(root, "linked/main.pdf", bytes.NewReader(payload), int64(len(payload)), hex.EncodeToString(digest[:]))
	if err == nil {
		t.Fatal("expected symlink parent rejection")
	}
	if _, err := os.Stat(filepath.Join(outside, "main.pdf")); !os.IsNotExist(err) {
		t.Fatalf("artifact escaped through symlink: %v", err)
	}
}

type tarEntry struct {
	name    string
	payload []byte
}

func buildResultArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.payload)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readCompileMultipart(r *http.Request) (protocol.CompileRequest, []string, error) {
	var request protocol.CompileRequest
	var files []string
	reader, err := r.MultipartReader()
	if err != nil {
		return request, nil, err
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return request, nil, err
		}
		switch part.FormName() {
		case "request":
			if err := json.NewDecoder(part).Decode(&request); err != nil {
				_ = part.Close()
				return request, nil, err
			}
		case "project":
			gz, err := gzip.NewReader(part)
			if err != nil {
				_ = part.Close()
				return request, nil, err
			}
			tr := tar.NewReader(gz)
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					_ = gz.Close()
					_ = part.Close()
					return request, nil, err
				}
				files = append(files, header.Name)
			}
			if err := gz.Close(); err != nil {
				_ = part.Close()
				return request, nil, err
			}
		default:
			if _, err := io.Copy(io.Discard, part); err != nil {
				_ = part.Close()
				return request, nil, err
			}
		}
		if err := part.Close(); err != nil {
			return request, nil, fmt.Errorf("close multipart part: %w", err)
		}
	}
	return request, files, nil
}
