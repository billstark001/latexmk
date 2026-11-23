package main

import (
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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func TestContinuousCompileDefaultEnvironment(t *testing.T) {
	const helperEnv = "LATEXMK_TEST_WATCH_CONTROL"
	if os.Getenv(helperEnv) == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Exit(run(os.Args[i+1:]))
			}
		}
		t.Fatal("missing helper arguments")
	}
	for _, mode := range []string{"--watch", "--realtime"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("LATEXMK_TOKEN", "")
			t.Setenv("LATEXMK_TOKEN_FILE", "")
			const source = `\documentclass{article}\begin{document}hello\end{document}`
			if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			environment := resources.Environment{Baseline: resources.Baseline{ID: "slim", Engines: []string{"xelatex"}}}
			var err error
			environment.Baseline.Digest, err = resources.BaselineDigest(environment.Baseline)
			if err != nil {
				t.Fatal(err)
			}
			environment.Digest, err = resources.EnvironmentDigest(environment)
			if err != nil {
				t.Fatal(err)
			}
			const pdf = "compiled PDF"
			pdfHash := sha256.Sum256([]byte(pdf))
			sourceHash := sha256.Sum256([]byte(source))
			result := protocol.CompileResult{
				ProtocolVersion:   protocol.Version,
				RequestID:         "job_control",
				EnvironmentDigest: environment.Digest,
				Entry:             "main.tex",
				Engine:            "xelatex",
				Success:           true,
				SourceRoot:        "/work/project",
				Artifacts: []protocol.Artifact{
					{Path: "main.pdf", Size: int64(len(pdf)), SHA256: hex.EncodeToString(pdfHash[:])},
				},
			}
			if mode == "--realtime" {
				result.SessionID, result.Revision = "ses_control", 1
			}
			archive := packArchive(t, result, map[string]string{"main.pdf": pdf})
			job := protocol.Job{ID: result.RequestID, SessionID: result.SessionID, Revision: result.Revision,
				Status: "succeeded", Result: &result}
			var resolved, prepared, uploaded, admitted, downloads atomic.Int32
			committed, downloaded := make(chan struct{}), make(chan struct{})
			var commitOnce, downloadOnce sync.Once
			session := func() protocol.Session {
				state := protocol.Session{ID: "ses_control"}
				if admitted.Load() > 0 {
					state.Revision, state.LatestJobID, state.LastSuccessfulJobID = 1, job.ID, job.ID
				}
				return state
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/meta":
					_ = json.NewEncoder(w).Encode(protocol.Metadata{ProtocolVersion: protocol.Version,
						Capabilities: protocol.Capabilities{ResourceEnvironments: true, RealtimeSessions: true,
							IncrementalUpload: true, QueuedJobs: true, MaxFiles: 100, MaxExpandedBytes: 1 << 20}})
				case "/v1/environments/resolve":
					var request resources.ResolveRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					} else if request.Baseline != "" || len(request.Resources) != 0 || len(request.Provided) != 0 {
						t.Errorf("ordinary project resolved explicit resources: %+v", request)
					}
					resolved.Add(1)
					_ = json.NewEncoder(w).Encode(environment)
				case "/v1/environments/" + environment.Digest + "/prepare":
					prepared.Add(1)
					ready := environment
					ready.Ready = true
					_ = json.NewEncoder(w).Encode(ready)
				case "/v1/uploads/plans":
					var request protocol.UploadPlanRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					} else if request.Request.EnvironmentDigest != environment.Digest || len(request.Files) != 1 ||
						request.Files[0].Path != "main.tex" || request.Files[0].SHA256 != hex.EncodeToString(sourceHash[:]) {
						t.Errorf("unexpected immutable source manifest: %+v", request)
					}
					_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_control",
						Missing: []string{hex.EncodeToString(sourceHash[:])}})
				case "/v1/uploads/upl_control/blobs/" + hex.EncodeToString(sourceHash[:]):
					data, err := io.ReadAll(r.Body)
					if err != nil || string(data) != source {
						t.Errorf("source upload = %q, %v", data, err)
					}
					uploaded.Add(1)
					w.WriteHeader(http.StatusNoContent)
				case "/v1/uploads/upl_control/commit", "/v1/sessions/ses_control/revisions":
					admitted.Add(1)
					commitOnce.Do(func() { close(committed) })
					_ = json.NewEncoder(w).Encode(job)
				case "/v1/jobs/job_control":
					_ = json.NewEncoder(w).Encode(job)
				case "/v1/jobs/job_control/result":
					_, _ = w.Write(archive)
					downloads.Add(1)
					downloadOnce.Do(func() { close(downloaded) })
				case "/v1/sessions", "/v1/sessions/ses_control", "/v1/sessions/ses_control/lease":
					if r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					_ = json.NewEncoder(w).Encode(session())
				case "/v1/sessions/ses_control/events":
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-committed:
					}
					_, _ = io.WriteString(w, "data: {\"sequence\":1,\"type\":\"finished\"}\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			// run owns signal contexts. A child process gives this complete-entry
			// regression a deadline without signaling other tests on failure.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			writerDone := make(chan error, 1)
			go func() {
				select {
				case <-ctx.Done():
					writerDone <- nil
					return
				case <-downloaded:
				}
				writerDone <- invalidateWatchConfiguration(ctx, root)
			}()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestContinuousCompileDefaultEnvironment$", "--",
				"latexmk", mode, "--server", server.URL, "--token-mode=none", "--project-id=paper",
				"--engine=xelatex", "--out-dir=out", "--timeout=2s", "--watch-interval=5ms",
				"--watch-debounce=10ms", "--watch-max-wait=30ms", "main.tex")
			command.Dir = root
			command.Env = append(os.Environ(), helperEnv+"=1")
			output, runErr := command.CombinedOutput()
			deadlineErr := ctx.Err()
			cancel()
			if err := <-writerDone; err != nil {
				t.Fatal(err)
			}
			log := string(output)
			output = output[max(0, len(output)-8192):]
			var exit *exec.ExitError
			if deadlineErr != nil || !errors.As(runErr, &exit) || exit.ExitCode() != 2 {
				t.Fatalf(
					"continuous compile did not exit through configuration reload: %v, %v\n%s",
					runErr,
					deadlineErr,
					output,
				)
			}
			started := "watching 1 selected files"
			if mode == "--realtime" {
				started = "submitted revision 1 (job_control)"
			}
			if strings.Contains(log, "watch target must have a name and path") ||
				!strings.Contains(log, "invalid or oversized JSON configuration") ||
				!strings.Contains(log, started) {
				t.Fatalf("continuous compile did not submit and reload settings:\n%s", output)
			}
			if resolved.Load() != 1 || prepared.Load() != 1 || uploaded.Load() == 0 || admitted.Load() == 0 ||
				downloads.Load() == 0 {
				t.Fatalf(
					"incomplete environment/compile lifecycle: resolve=%d prepare=%d upload=%d admit=%d download=%d",
					resolved.Load(),
					prepared.Load(),
					uploaded.Load(),
					admitted.Load(),
					downloads.Load(),
				)
			}
			if _, err := os.Stat(filepath.Join(root, cliresources.DefaultLockFile)); !os.IsNotExist(err) {
				t.Fatalf("ordinary project created a resource lock: %v", err)
			}
		})
	}
}

func invalidateWatchConfiguration(ctx context.Context, root string) error {
	fs, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer func() { _ = fs.Close() }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for generation := 1; ; generation++ {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Watch initializes its tracker after downloading the first result.
			// Keep changing atomic snapshots until an initialized tracker reloads.
			_, err := fs.WriteAtomic(".latexmk.json", 100, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "{invalid configuration %d", generation)
				return err
			})
			if err != nil {
				return err
			}
		}
	}
}
