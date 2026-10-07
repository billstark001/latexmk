package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestRealtimeRetriesTransientDiagnosticDownload(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	result := protocol.CompileResult{
		ProtocolVersion: protocol.Version,
		RequestID:       "job_failed",
		SessionID:       "ses_test",
		Revision:        1,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Error:           "diagnostic recovered",
	}
	job := protocol.Job{
		ID:        result.RequestID,
		SessionID: result.SessionID,
		Revision:  1,
		Status:    "failed",
		Result:    &result,
	}
	session := protocol.Session{ID: result.SessionID, Revision: 1, LatestJobID: job.ID}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(
		&tar.Header{Name: "result.json", Mode: 0600, Size: int64(len(raw)), Typeflag: tar.TypeReg},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(raw); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	var jobReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/uploads/plans":
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_test"})
		case "/v1/sessions/ses_test":
			_ = json.NewEncoder(w).Encode(session)
		case "/v1/sessions/ses_test/revisions":
			_ = json.NewEncoder(w).Encode(job)
		case "/v1/jobs/job_failed":
			jobReads.Add(1)
			_ = json.NewEncoder(w).Encode(job)
		case "/v1/jobs/job_failed/result":
			if downloads.Add(1) == 1 {
				http.Error(w, "temporary failure", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(archive.Bytes())
		case "/v1/sessions/ses_test/events":
			w.Header().Set("Content-Type", "text/event-stream")
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for sequence := 1; ; sequence++ {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = fmt.Fprintf(
						w,
						"event: session\nid: %d\ndata: {\"sequence\":%d,\"type\":\"finished\",\"status\":\"failed\"}\n\n",
						sequence,
						sequence,
					)
					w.(http.Flusher).Flush()
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := client.New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.ProjectID, c.UploadMode = root, "paper", "all"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, stderr := captureCommandOutput(t, func() int {
		return runLiveSession(
			ctx,
			c,
			protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"},
			compileOptions{timeout: time.Second, outDir: t.TempDir()},
			protocol.Metadata{Capabilities: protocol.Capabilities{MaxFiles: 10, MaxExpandedBytes: 1024}},
			session,
			liveObservation{},
		)
	})
	if downloads.Load() != 2 || !strings.Contains(stderr, result.Error) {
		t.Fatalf("diagnostics were not recovered exactly once: downloads=%d, stderr=%s", downloads.Load(), stderr)
	}
	if jobReads.Load() != 2 {
		t.Fatalf("already processed failure was fetched again: reads=%d", jobReads.Load())
	}
}

func TestRealtimeSettingsChangeCancelsInFlightOperations(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, ".latexmk.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New("http://127.0.0.1:8080", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.UploadMode = root, "all"
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	observation := observeLive(
		ctx,
		c,
		protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"},
		compileOptions{
			projectRoot:   root,
			watchInterval: 20 * time.Millisecond,
			watchDebounce: 10 * time.Millisecond,
			controlFiles:  []string{cfg},
		},
		cancel,
	)
	if err := os.WriteFile(cfg, []byte("{\"outDir\":\"changed\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errLiveSettingsChanged) {
			t.Fatal(context.Cause(ctx))
		}
	case err := <-observation.errors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("settings change left upload context active")
	}
}

func TestCompilerReportDoesNotExecuteTerminalControls(t *testing.T) {
	_, stdout, stderr := captureCommandOutput(t, func() int {
		return reportCompile(
			client.CompileOutput{
				Result: protocol.CompileResult{Success: true},
				Stdout: []byte("中文\n\x1b]52;clipboard\a\r\u009bunsafe"),
				Stderr: []byte("\x1b[2Jerror"),
			},
			nil,
			compileOptions{},
		)
	})
	if strings.ContainsAny(stdout+stderr, "\x1b\a\r\u009b") || !strings.Contains(stdout, "中文\n") {
		t.Fatalf("unsafe terminal text: %q %q", stdout, stderr)
	}
}
