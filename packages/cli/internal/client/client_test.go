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

func TestArtifactListAndDownloadUseOpaqueIDAndHash(t *testing.T) {
	pdf := []byte("pdf payload")
	logData := []byte("compiler log")
	pdfHash := sha256.Sum256(pdf)
	logHash := sha256.Sum256(logData)
	result := protocol.CompileResult{ProtocolVersion: 2, RequestID: "job_result", Success: true, Artifacts: []protocol.Artifact{
		{Path: "main.log", Size: int64(len(logData)), SHA256: hex.EncodeToString(logHash[:])},
		{Path: "main.pdf", Size: int64(len(pdf)), SHA256: hex.EncodeToString(pdfHash[:])},
	}}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	archive := buildResultArchive(t, []tarEntry{
		{name: "result.json", payload: resultJSON},
		{name: "stdout.log", payload: nil},
		{name: "stderr.log", payload: nil},
		{name: "artifacts/main.log", payload: logData},
		{name: "artifacts/main.pdf", payload: pdf},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_result":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_result", Status: "succeeded", Result: &result})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_result/result":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := c.ListArtifacts(context.Background(), "job_result")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 || len(artifacts[1].ID) != 32 || artifacts[1].Path != "main.pdf" || artifacts[1].MIMEType != "application/pdf" {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	root := filepath.Join(t.TempDir(), "output")
	download, err := c.DownloadArtifact(context.Background(), "job_result", artifacts[1].ID, root)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(download.LocalPath)
	if err != nil || !bytes.Equal(installed, pdf) {
		t.Fatalf("installed artifact = %q, %v", installed, err)
	}
	if _, err := os.Stat(filepath.Join(root, "main.log")); !os.IsNotExist(err) {
		t.Fatalf("unselected artifact was installed: %v", err)
	}
}

func TestLogsShareBoundedTailBudgetAcrossSources(t *testing.T) {
	stdout := []byte("one\ntwo\nthree\n")
	stderr := []byte("err1\nerr2\n")
	compiler := []byte("a\nb\nc\nd\n")
	compilerHash := sha256.Sum256(compiler)
	result := protocol.CompileResult{ProtocolVersion: 2, RequestID: "job_logs", Success: false, Artifacts: []protocol.Artifact{
		{Path: "main.log", Size: int64(len(compiler)), SHA256: hex.EncodeToString(compilerHash[:])},
	}}
	resultJSON, _ := json.Marshal(result)
	archive := buildResultArchive(t, []tarEntry{
		{name: "result.json", payload: resultJSON},
		{name: "stdout.log", payload: stdout},
		{name: "stderr.log", payload: stderr},
		{name: "artifacts/main.log", payload: compiler},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			_, _ = w.Write(archive)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_logs", Status: "failed", Result: &result})
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := c.Logs(context.Background(), "job_logs", "all", 2, 18)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs.Entries) != 3 || logs.Returned > 18 || !logs.ArchiveDone {
		t.Fatalf("logs = %#v", logs)
	}
	for _, entry := range logs.Entries {
		if entry.ReturnedBytes > 6 || entry.ReturnedBytes == 0 {
			t.Fatalf("log budget was not shared: %#v", logs.Entries)
		}
	}
	if logs.Entries[2].Source != "compiler" || logs.Entries[2].Path != "main.log" {
		t.Fatalf("compiler log = %#v", logs.Entries[2])
	}
}

func TestDiagnosticsIndexKeepsRawLogLocations(t *testing.T) {
	stdout := []byte("This is pdfTeX\n(./main.tex\n./main.tex:7: Undefined control sequence.\nl.7 \\badcommand\nLaTeX Warning: Citation `missing' undefined on input line 12.\n")
	compiler := []byte("(./main.tex\n./main.tex:7: Undefined control sequence.\nl.7 \\badcommand\nLaTeX Warning: Citation `missing' undefined on input line 12.\n")
	compilerHash := sha256.Sum256(compiler)
	result := protocol.CompileResult{ProtocolVersion: 2, RequestID: "job_diagnostics", Success: false, Artifacts: []protocol.Artifact{
		{Path: "main.log", Size: int64(len(compiler)), SHA256: hex.EncodeToString(compilerHash[:])},
	}}
	resultJSON, _ := json.Marshal(result)
	archive := buildResultArchive(t, []tarEntry{
		{name: "result.json", payload: resultJSON},
		{name: "stdout.log", payload: stdout},
		{name: "stderr.log", payload: nil},
		{name: "artifacts/main.log", payload: compiler},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			_, _ = w.Write(archive)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_diagnostics", Status: "failed", Result: &result})
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	output, err := c.Diagnostics(context.Background(), "job_diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	if output.Count != 2 || output.Incomplete || len(output.LogsScanned) != 3 {
		t.Fatalf("diagnostics output = %#v", output)
	}
	errorDiagnostic := output.Diagnostics[0]
	if errorDiagnostic.Severity != "error" || errorDiagnostic.File != "main.tex" || errorDiagnostic.FileInferred || errorDiagnostic.Line != 7 || errorDiagnostic.Context != `\badcommand` {
		t.Fatalf("error diagnostic = %#v", errorDiagnostic)
	}
	if len(errorDiagnostic.LogLocations) != 2 || errorDiagnostic.LogLocations[0] != (LogLocation{Source: "stdout", Path: "stdout.log", StartLine: 3, EndLine: 4}) || errorDiagnostic.LogLocations[1] != (LogLocation{Source: "compiler", Path: "main.log", StartLine: 2, EndLine: 3}) {
		t.Fatalf("error locations = %#v", errorDiagnostic.LogLocations)
	}
	warning := output.Diagnostics[1]
	if warning.Severity != "warning" || warning.File != "main.tex" || !warning.FileInferred || warning.Line != 12 || len(warning.LogLocations) != 2 {
		t.Fatalf("warning diagnostic = %#v", warning)
	}
}

func TestDiagnosticStreamBoundsUntrustedLines(t *testing.T) {
	parser := newDiagnosticStream("compiler", "main.log")
	longLine := "! " + strings.Repeat("x", maxDiagnosticLineBytes+1024) + "\n"
	if _, err := parser.Write([]byte(longLine)); err != nil {
		t.Fatal(err)
	}
	parser.finish()
	if !parser.incomplete || parser.oversizedLines != 1 || len(parser.diagnostics) != 1 {
		t.Fatalf("parser state = %#v", parser)
	}
	if len(parser.diagnostics[0].Message) > maxDiagnosticTextBytes+len("…") {
		t.Fatalf("diagnostic message was not bounded: %d", len(parser.diagnostics[0].Message))
	}
}

func TestDiagnosticStreamIndexesClassicTeXErrorAsInferred(t *testing.T) {
	parser := newDiagnosticStream("compiler", "main.log")
	logData := "(./chapters/body.tex\n! LaTeX Error: File `missing.sty' not found.\n\nl.23 \\usepackage{missing}\n"
	if _, err := parser.Write([]byte(logData)); err != nil {
		t.Fatal(err)
	}
	parser.finish()
	if len(parser.diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", parser.diagnostics)
	}
	diagnostic := parser.diagnostics[0]
	if diagnostic.File != "chapters/body.tex" || !diagnostic.Inferred || diagnostic.Line != 23 || diagnostic.Context != `\usepackage{missing}` {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if diagnostic.Location != (LogLocation{Source: "compiler", Path: "main.log", StartLine: 2, EndLine: 4}) {
		t.Fatalf("location = %#v", diagnostic.Location)
	}
}

func TestDiagnosticFileDoesNotExposeAbsoluteSystemPaths(t *testing.T) {
	if got := diagnosticFile("/tmp/latexmk-job-123/project/sections/body.tex"); got != "sections/body.tex" {
		t.Fatalf("workspace file = %q", got)
	}
	if got := diagnosticFile("/usr/local/texlive/texmf-dist/tex/latex/base/article.cls"); got != "" {
		t.Fatalf("system path exposed as diagnostic file: %q", got)
	}
	if got := diagnosticFile(`C:\\texlive\\texmf-dist\\article.cls`); got != "" {
		t.Fatalf("Windows system path exposed as diagnostic file: %q", got)
	}
	if got := diagnosticFile(`..\\secret.tex`); got != "" {
		t.Fatalf("traversal exposed as diagnostic file: %q", got)
	}
}

func TestResultStateErrorMarksQueuedResultUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_wait", Status: "running"})
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListArtifacts(context.Background(), "job_wait")
	var stateErr *ResultStateError
	if !errors.As(err, &stateErr) || stateErr.Status != "running" {
		t.Fatalf("error = %#v", err)
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

func TestStartCompileReturnsImmutableJobWithoutPolling(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	var planned protocol.UploadPlanRequest
	requests := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 2, Capabilities: protocol.Capabilities{IncrementalUpload: true, QueuedJobs: true, DependencyInputs: true, NeedsFiles: true}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/uploads/plans":
			if err := json.NewDecoder(r.Body).Decode(&planned); err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_detach", Missing: []string{planned.Files[0].SHA256}, ExpiresAt: time.Now().Add(time.Minute)})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/uploads/upl_detach/blobs/"):
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/uploads/upl_detach/commit":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_detach", ProjectID: planned.ProjectID, SnapshotID: "snap_detach", Status: "queued", CreatedAt: time.Now()})
		default:
			t.Errorf("detached compile made unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", 3*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	out, err := c.StartCompile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Job.ID != "job_detach" || out.Job.SnapshotID != "snap_detach" || out.Job.Status != "queued" {
		t.Fatalf("detached job = %#v", out.Job)
	}
	if !planned.Request.RecordInputs || !planned.Request.DetectMissingFiles {
		t.Fatalf("detached request did not negotiate capabilities: %#v", planned.Request)
	}
	if len(requests) != 4 {
		t.Fatalf("detached compile polled or downloaded a result: %#v", requests)
	}
}

func TestStartCompileRequiresQueuedCapabilities(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 1})
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = root
	_, err = c.StartCompile(context.Background(), protocol.CompileRequest{ProtocolVersion: protocol.Version, Entry: "main.tex", Engine: "xelatex"})
	var capabilityErr *CapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Capability != "detached queued compilation" {
		t.Fatalf("error = %#v", err)
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

func TestCompileCacheRequiresAdvertisedCapability(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(fmt.Sprint(detached), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/meta" {
					t.Errorf("unexpected request before capability check: %s", r.URL.Path)
					http.Error(w, "unexpected", 500)
					return
				}
				json.NewEncoder(w).Encode(protocol.Metadata{Capabilities: protocol.Capabilities{QueuedJobs: true, IncrementalUpload: true}})
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			c.ProjectRoot = t.TempDir()
			if err := os.WriteFile(filepath.Join(c.ProjectRoot, "main.tex"), []byte("hello"), 0600); err != nil {
				t.Fatal(err)
			}
			req := protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex", Auxiliary: protocol.AuxiliaryOptions{Server: "reuse"}}
			if detached {
				_, err = c.StartCompile(context.Background(), req)
			} else {
				_, err = c.Compile(context.Background(), req, t.TempDir())
			}
			var capability *CapabilityError
			if !errors.As(err, &capability) {
				t.Fatalf("expected capability error, got %v", err)
			}
		})
	}
}
