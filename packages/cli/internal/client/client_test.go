package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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
	resultArchive := buildResultArchive(t, []tarEntry{{name: "result.json", payload: []byte(`{"protocolVersion":2,"requestId":"job_test","success":true,"exitCode":0}`)}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: 2, Capabilities: protocol.Capabilities{IncrementalUpload: true, QueuedJobs: true}})
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
	c, err := New(server.URL, "", 3*time.Second, false, "")
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
	c, err := New(server.URL, "", 3*time.Second, false, "")
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
