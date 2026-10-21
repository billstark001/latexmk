package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

// Publish revision one only after revision two has been captured. The exported
// filename must still come from revision one's clock, including commit replay.
func TestRealtimeBuildNamingBelongsToPublishedRevision(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "main.tex")
	if err := os.WriteFile(entry, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int32
	clock.Store(1)
	invocation := newNameInvocation()
	invocation.now = func() time.Time { return time.Date(2026, 10, 22, 0, 0, int(clock.Load()), 0, time.UTC) }
	opts := compileOptions{projectRoot: root, entry: "main.tex", engine: "xelatex", outDir: root,
		pdfTemplate: "exports/{datetime:go:150405}.pdf", nameScope: "build", nameTimeZone: "UTC",
		invocation: invocation, timeout: time.Second}
	if err := preparePDFNaming(&opts); err != nil {
		t.Fatal(err)
	}
	pdf := "verified pdf"
	digest := sha256.Sum256([]byte(pdf))
	result := protocol.CompileResult{
		ProtocolVersion: protocol.Version,
		RequestID:       "job_1",
		SessionID:       "ses_test",
		Revision:        1,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Success:         true,
		SourceRoot:      "/work/project",
		Artifacts: []protocol.Artifact{
			{Path: "main.pdf", Size: int64(len(pdf)), SHA256: hex.EncodeToString(digest[:])},
		},
	}
	archive := packArchive(t, result, map[string]string{"main.pdf": pdf})
	session := protocol.Session{ID: "ses_test"}
	var mu sync.Mutex
	var commits atomic.Int32
	observation := liveObservation{changed: make(chan struct{}, 1)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/uploads/plans":
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_test"})
		case "/v1/sessions/ses_test":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(session)
		case "/v1/sessions/ses_test/revisions":
			// Lose the first response without admitting it, exercising the
			// pending operation path while the clock changes.
			n := commits.Add(1)
			if n == 1 {
				clock.Store(2)
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
			revision := uint64(n - 1)
			job := protocol.Job{
				ID:        fmt.Sprintf("job_%d", revision),
				SessionID: "ses_test",
				Revision:  revision,
				Status:    "running",
			}
			mu.Lock()
			session.Revision, session.LatestJobID, session.RunningJobID = revision, job.ID, job.ID
			if revision == 2 {
				session.LastSuccessfulJobID = "job_1"
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(job)
			if revision == 1 {
				if err := os.WriteFile(entry, []byte("two"), 0600); err != nil {
					t.Error(err)
				}
				observation.changed <- struct{}{}
			}
		case "/v1/jobs/job_1":
			_ = json.NewEncoder(
				w,
			).Encode(
				protocol.Job{ID: "job_1", SessionID: "ses_test", Revision: 1, Status: "succeeded", Result: &result},
			)
		case "/v1/jobs/job_2":
			_ = json.NewEncoder(
				w,
			).Encode(
				protocol.Job{ID: "job_2", SessionID: "ses_test", Revision: 2, Status: "running"},
			)
		case "/v1/jobs/job_1/result":
			_, _ = w.Write(archive)
		case "/v1/sessions/ses_test/events":
			w.Header().Set("Content-Type", "text/event-stream")
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for sequence := 1; ; sequence++ {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					_, _ = fmt.Fprintf(w, "data: {\"sequence\":%d,\"type\":\"finished\"}\n\n", sequence)
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
	c.GeneratedFiles = generatedFiles(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// A transient commit is normally retried by the poll ticker. Explicit
	// source notifications let this test exercise that path without waiting 5s.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if commits.Load() == 1 {
					select {
					case observation.changed <- struct{}{}:
					default:
					}
				}
				if _, err := os.Stat(filepath.Join(root, "exports/000001.pdf")); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	code, _, stderr := captureCommandOutput(t, func() int {
		return runLiveSession(ctx, c,
			protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"}, opts,
			protocol.Metadata{Capabilities: protocol.Capabilities{MaxFiles: 100, MaxExpandedBytes: 1 << 20}},
			protocol.Session{ID: "ses_test"}, observation)
	})
	<-done
	data, err := os.ReadFile(filepath.Join(root, "exports/000001.pdf"))
	if err != nil || string(data) != pdf || code != 0 {
		t.Fatalf("revision filename lost: code=%d pdf=%q err=%v log=%s", code, data, err, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "exports/000002.pdf")); !os.IsNotExist(err) {
		t.Fatal("old result used the newer naming context", err)
	}
}

func TestRealtimeNoOpDoesNotCaptureOrRetainBuildNames(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	invocation := newNameInvocation()
	var captures, selections, commits atomic.Int32
	invocation.now = func() time.Time { return time.Unix(int64(captures.Add(1)), 0).UTC() }
	opts := compileOptions{
		projectRoot:  root,
		entry:        "main.tex",
		outDir:       root,
		invocation:   invocation,
		nameScope:    "build",
		pdfTemplate:  "{datetime:go:150405}.pdf",
		nameTimeZone: "UTC",
		timeout:      time.Second,
	}
	if err := preparePDFNaming(&opts); err != nil {
		t.Fatal(err)
	}
	defer opts.naming.previewLease.Close()
	job := protocol.Job{ID: "job_1", SessionID: "ses_test", Revision: 1, Status: "running"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/uploads/plans":
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_1"})
		case "/v1/sessions/ses_test":
			state := protocol.Session{ID: "ses_test"}
			if commits.Load() > 0 {
				state.Revision, state.RunningJobID, state.LatestJobID = 1, "job_1", "job_1"
			}
			_ = json.NewEncoder(w).Encode(state)
		case "/v1/sessions/ses_test/revisions":
			commits.Add(1)
			_ = json.NewEncoder(w).Encode(job)
		case "/v1/sessions/ses_test/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
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
	c.GeneratedFiles = func() ([]string, error) { selections.Add(1); return generatedFiles(opts)() }
	observation := liveObservation{changed: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if selections.Load() >= 20 {
					cancel()
					return
				}
				if commits.Load() > 0 {
					select {
					case observation.changed <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	code, _, stderr := captureCommandOutput(t, func() int {
		return runLiveSession(
			ctx,
			c,
			protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"},
			opts,
			protocol.Metadata{
				Capabilities: protocol.Capabilities{MaxFiles: 100, MaxExpandedBytes: 1 << 20},
			},
			protocol.Session{ID: "ses_test"},
			observation,
		)
	})
	<-done
	if code != 0 || selections.Load() < 20 || commits.Load() != 1 || captures.Load() != 2 {
		t.Fatalf(
			"no-op became a build: code=%d selections=%d commits=%d captures=%d log=%s",
			code,
			selections.Load(),
			commits.Load(),
			captures.Load(),
			stderr,
		)
	}
	if len(opts.generated.snapshot()) != 1 {
		t.Fatal("session exit retained build reservations", opts.generated.snapshot())
	}
}
