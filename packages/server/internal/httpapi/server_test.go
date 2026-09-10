package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/jobs"
	"github.com/billstark001/latexmk/packages/server/internal/project"
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
	queue := jobs.New(cfg, api.Metadata{}, runner, projects, nil, logger)
	return New(cfg, api.Metadata{}, runner, auth.New(cfg, nil), nil, projects, queue, logger)
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
