package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func readLiveTiming(t *testing.T, output string) []liveTimingEvent {
	t.Helper()
	var events []liveTimingEvent
	for _, line := range strings.Split(output, "\n") {
		payload, ok := strings.CutPrefix(line, "realtime_diagnostics ")
		if !ok {
			continue
		}
		var event liveTimingEvent
		decoder := json.NewDecoder(strings.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			t.Fatalf("invalid diagnostic line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestLiveTimingRequiresExplicitOptIn(t *testing.T) {
	for _, setting := range []string{"", "0", "true", "1"} {
		t.Run("value="+setting, func(t *testing.T) {
			t.Setenv("LATEXMK_REALTIME_DIAGNOSTICS", setting)
			_, stdout, stderr := captureCommandOutput(t, func() int {
				timing := newLiveTiming()
				if (timing != nil) != (setting == "1") {
					t.Fatalf("unexpected opt-in state for %q", setting)
				}
				timing.record(timing.start(), liveTimingEvent{Event: "metadata", OK: true})
				return 0
			})
			if stdout != "" {
				t.Fatalf("diagnostics wrote stdout: %q", stdout)
			}
			if setting == "1" {
				if events := readLiveTiming(t, stderr); len(events) != 1 || events[0].Event != "metadata" {
					t.Fatalf("missing enabled diagnostics: %q", stderr)
				}
			} else if stderr != "" {
				t.Fatalf("disabled diagnostics wrote stderr: %q", stderr)
			}
		})
	}
}

func TestLiveTimingConcurrentLinesAndElapsedDuration(t *testing.T) {
	var output bytes.Buffer
	timing := &liveTiming{writer: &output}
	const count = 200
	var writers sync.WaitGroup
	for attempt := range uint64(count) {
		writers.Go(func() {
			timing.record(timing.start(), liveTimingEvent{
				Event: "capture", SessionID: "ses_concurrent", Attempt: attempt + 1, OK: attempt%2 == 0,
			})
		})
	}
	writers.Wait()
	events := readLiveTiming(t, output.String())
	if len(events) != count || strings.Count(output.String(), "\n") != count {
		t.Fatalf("concurrent writes lost or split records: %d", len(events))
	}
	seen := make(map[uint64]bool, count)
	for _, event := range events {
		if seen[event.Attempt] || event.Event != "capture" || event.SessionID != "ses_concurrent" ||
			event.DurationNS < 0 || event.TimeNS == 0 || event.OK != ((event.Attempt-1)%2 == 0) {
			t.Fatalf("invalid concurrent record: %+v", event)
		}
		seen[event.Attempt] = true
	}
	output.Reset()
	started := timing.start()
	minimum := time.Since(started).Nanoseconds()
	timing.record(started, liveTimingEvent{Event: "upload", OK: true})
	maximum := time.Since(started).Nanoseconds()
	measured := readLiveTiming(t, output.String())[0].DurationNS
	if measured < minimum || measured > maximum {
		t.Fatalf("duration %d outside monotonic elapsed bounds [%d,%d]", measured, minimum, maximum)
	}
}

func TestLiveTimingCorrelatesSubmissionAndPublicationWithoutSourceData(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			setting := "0"
			if enabled {
				setting = "1"
			}
			t.Setenv("LATEXMK_REALTIME_DIAGNOSTICS", setting)
			root := t.TempDir()
			const entry = "private-source.tex"
			const source = "PRIVATE SOURCE CONTENT"
			const token = "private-credential"
			if err := os.WriteFile(filepath.Join(root, entry), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			pdf := "compiled pdf"
			digest := sha256.Sum256([]byte(pdf))
			result := protocol.CompileResult{
				ProtocolVersion: protocol.Version, RequestID: "job_timing", SessionID: "ses_timing", Revision: 1,
				Entry: entry, Engine: "xelatex", Success: true, SourceRoot: "/work/project",
				Artifacts: []protocol.Artifact{
					{Path: "private-source.pdf", Size: int64(len(pdf)), SHA256: hex.EncodeToString(digest[:])},
				},
			}
			job := protocol.Job{
				ID:        result.RequestID,
				SessionID: result.SessionID,
				Revision:  1,
				Status:    "succeeded",
				Result:    &result,
			}
			archive := packArchive(t, result, map[string]string{"private-source.pdf": pdf})
			finishedEvent, err := json.Marshal(protocol.SessionEvent{
				Sequence: 1, Type: "finished", JobID: job.ID, Revision: job.Revision,
			})
			if err != nil {
				t.Fatal(err)
			}
			var admitted atomic.Bool
			committed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/uploads/plans":
					_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_timing"})
				case "/v1/sessions/ses_timing/revisions":
					if !admitted.Swap(true) {
						close(committed)
					}
					_ = json.NewEncoder(w).Encode(job)
				case "/v1/sessions/ses_timing":
					session := protocol.Session{ID: "ses_timing"}
					if admitted.Load() {
						session.Revision, session.LatestJobID, session.LastSuccessfulJobID = 1, job.ID, job.ID
					}
					_ = json.NewEncoder(w).Encode(session)
				case "/v1/jobs/job_timing":
					_ = json.NewEncoder(w).Encode(job)
				case "/v1/jobs/job_timing/result":
					_, _ = w.Write(archive)
				case "/v1/sessions/ses_timing/events":
					select {
					case <-r.Context().Done():
						return
					case <-committed:
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: session\nid: 1\ndata: %s\n\n", finishedEvent)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c, err := client.New(server.URL, token, time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			c.ProjectRoot, c.ProjectID, c.UploadMode = root, "paper", "all"
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			exported := make(chan struct{})
			go func() {
				defer close(exported)
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if _, err := os.Stat(filepath.Join(root, "export-private.pdf")); err == nil {
							cancel()
							return
						}
					}
				}
			}()
			code, stdout, stderr := captureCommandOutput(t, func() int {
				return runLiveSession(
					ctx,
					c,
					protocol.CompileRequest{Entry: entry, Engine: "xelatex"},
					compileOptions{
						entry:       entry,
						projectRoot: root,
						outDir:      t.TempDir(),
						pdfExport:   "export-private.pdf",
						timeout:     time.Second,
					},
					protocol.Metadata{Capabilities: protocol.Capabilities{MaxFiles: 10, MaxExpandedBytes: 1024}},
					protocol.Session{ID: "ses_timing"},
					liveObservation{timing: newLiveTiming()},
				)
			})
			<-exported
			if code != 0 || strings.Contains(stdout, "realtime_diagnostics") {
				t.Fatalf("timing changed command output: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if data, err := os.ReadFile(filepath.Join(root, "export-private.pdf")); err != nil || string(data) != pdf {
				t.Fatalf("publication failed: %q, %v; stderr=%s", data, err, stderr)
			}
			events := readLiveTiming(t, stderr)
			if !enabled {
				if len(events) != 0 {
					t.Fatal("disabled session emitted diagnostics")
				}
				return
			}
			byName := make(map[string]liveTimingEvent, len(events))
			for _, event := range events {
				if !event.OK || event.SessionID != "ses_timing" || event.DurationNS < 0 {
					t.Fatalf("invalid session record: %+v", event)
				}
				byName[event.Event] = event
			}
			for _, name := range []string{"submit_start", "capture", "submit_state", "upload", "commit", "event_received", "report_state", "result_metadata", "download_publish", "export"} {
				if _, ok := byName[name]; !ok {
					t.Fatalf("missing phase %s: %s", name, stderr)
				}
			}
			for _, name := range []string{"capture", "submit_state", "upload", "commit"} {
				if byName[name].Attempt != byName["submit_start"].Attempt || byName[name].DurationNS <= 0 {
					t.Fatalf("phase not linked to measured submit: %+v", byName[name])
				}
			}
			for _, name := range []string{"commit", "event_received", "result_metadata", "download_publish", "export"} {
				if event := byName[name]; event.JobID != job.ID || event.Revision != 1 {
					t.Fatalf("phase not linked to published job: %+v", event)
				}
			}
			for _, line := range strings.Split(stderr, "\n") {
				if !strings.HasPrefix(line, "realtime_diagnostics ") {
					continue
				}
				for _, private := range []string{root, entry, source, token, "export-private.pdf", "/work/project"} {
					if strings.Contains(line, private) {
						t.Fatalf("diagnostic contains private input %q: %s", private, line)
					}
				}
			}
		})
	}
}
