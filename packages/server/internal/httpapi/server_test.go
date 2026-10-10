package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/jobs"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func newTestServer(t *testing.T, legacy bool) *Server {
	t.Helper()
	cfg := config.Config{
		AuthMode:              "none",
		StateDir:              t.TempDir(),
		Engines:               []string{"xelatex"},
		MaxFiles:              10,
		MaxUploadBytes:        1024,
		MaxExpandedBytes:      1024,
		MaxStateBytes:         4096,
		MaxQueuedJobs:         2,
		MaxConcurrentCompiles: 1,
		EnableLegacyCompile:   legacy,
	}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := compile.NewRunner(cfg)
	queue := jobs.New(cfg, protocol.Metadata{}, runner, projects, nil, logger)
	return New(cfg, protocol.Metadata{}, runner, auth.New(cfg, nil), nil, projects, queue, logger)
}

func TestLegacyCompileRouteDisabledByDefault(t *testing.T) {
	server := newTestServer(t, false)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/compile", nil)
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestCleanupDeleteRequiresPreviewDigest(t *testing.T) {
	server := newTestServer(t, false)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/projects/project-test/cleanup?scope=project", nil)
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionLeaseEndpointAndAbandonedEventStream(t *testing.T) {
	server := newTestServer(t, false)
	cfg := server.cfg
	cfg.MaxRealtimeSessions, cfg.MaxRealtimeSessionsPerOwner = 4, 4
	cfg.RealtimeSessionTTL, cfg.ShutdownTimeout = 200*time.Millisecond, time.Second
	server.jobs = jobs.New(cfg, server.meta, server.runner, server.projects, nil, server.logger)
	ctx, cancel := context.WithCancel(context.Background())
	server.jobs.Start(ctx)
	defer func() {
		cancel()
		if err := server.jobs.Wait(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	request := protocol.SessionRequest{
		ProjectID: "paper", Workspace: "fresh", IdempotencyKey: "session-http-000001",
		Request: protocol.CompileRequest{ProtocolVersion: protocol.Version,
			Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(raw)))
	var session protocol.Session
	if recorder.Code != http.StatusCreated || json.Unmarshal(recorder.Body.Bytes(), &session) != nil {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+session.ID+"/lease", nil))
	var renewed protocol.Session
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &renewed) != nil ||
		!renewed.ExpiresAt.After(session.ExpiresAt) {
		t.Fatalf("renew: %d %s", recorder.Code, recorder.Body.String())
	}
	transport := httptest.NewServer(server.Handler())
	defer transport.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(transport.URL + "/v1/sessions/" + session.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	// Continue consuming upstream bytes as a proxy would. A subscriber alone
	// cannot keep the lease alive and expiry must close the HTTP stream.
	if response.StatusCode != http.StatusOK {
		t.Fatalf("subscribe: %d", response.StatusCode)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("abandoned stream outlived lease: %v", err)
	}
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/sessions/"+session.ID+"/lease", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expired renewal: %d", recorder.Code)
	}
}
