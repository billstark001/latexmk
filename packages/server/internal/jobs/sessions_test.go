package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/project"
)

func sessionManager(t *testing.T) (*Manager, api.SessionRequest) {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir(), TempDir: t.TempDir(), Engines: []string{"xelatex"}, MaxFiles: 100, MaxExpandedBytes: 8192, MaxUploadBytes: 8192, MaxStateBytes: 1 << 20, MaxArtifactBytes: 8192, MaxLogBytes: 8192, MaxConcurrentCompiles: 2, MaxQueuedJobs: 2, CompileTimeout: 3 * time.Second, ShutdownTimeout: time.Second, MaxRealtimeSessions: 4, RealtimeSessionTTL: time.Minute}
	p, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg, api.Metadata{}, compile.NewRunner(cfg), p, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, api.SessionRequest{ProjectID: "paper", Workspace: "fresh", Request: api.CompileRequest{ProtocolVersion: 2, Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"}}
}

func planRevision(t *testing.T, m *Manager, req api.SessionRequest, content string, base uint64) api.RevisionRequest {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	plan, err := m.projects.Plan("owner", api.UploadPlanRequest{ProjectID: req.ProjectID, Request: req.Request, Files: []api.ProjectFile{{Path: "main.tex", SHA256: hash, Size: int64(len(content))}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range plan.Missing {
		if err := m.projects.PutBlob("owner", plan.UploadID, missing, bytes.NewBufferString(content)); err != nil {
			t.Fatal(err)
		}
	}
	return api.RevisionRequest{UploadID: plan.UploadID, BaseRevision: base, IdempotencyKey: fmt.Sprintf("revision-key-%016d", base)}
}

func TestSessionCoalescesWithoutGrowingQueueAndReplaysReceipts(t *testing.T) {
	m, req := sessionManager(t)
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	var last api.Job
	var submitted api.RevisionRequest
	for i := uint64(0); i < 150; i++ {
		submitted = planRevision(t, m, req, fmt.Sprintf("version %d", i), i)
		job, err := m.SubmitRevision(ctx, "owner", s.ID, submitted)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			previous, _ := m.Get(ctx, "owner", last.ID)
			if previous.Status != "cancelled" {
				t.Fatalf("old pending status: %s", previous.Status)
			}
		}
		last = job
	}
	if len(m.queue) != 1 {
		t.Fatalf("queue tokens=%d", len(m.queue))
	}
	repeated, err := m.SubmitRevision(ctx, "owner", s.ID, submitted)
	if err != nil || repeated.ID != last.ID {
		t.Fatalf("retry=%+v %v", repeated, err)
	}
	submitted.UploadID = "different"
	if _, err := m.SubmitRevision(ctx, "owner", s.ID, submitted); err == nil {
		t.Fatal("reused key accepted a different operation")
	}
	submitted.IdempotencyKey = "another-key-00000001"
	if _, err := m.SubmitRevision(ctx, "owner", s.ID, submitted); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error: %v", err)
	}
	events, _, err := m.SessionEvents("owner", s.ID, 0)
	if err != nil || len(events) != 1 || events[0].Type != "resync" {
		t.Fatalf("replay=%+v %v", events, err)
	}
	if _, err := m.GetSession(ctx, "other-owner", s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("session crossed owner boundary")
	}
	preview, err := m.CleanupProject(ctx, "owner", "paper", "project")
	if err != nil || len(preview.ActiveSessions) != 1 {
		t.Fatalf("cleanup preview=%+v %v", preview, err)
	}
	if _, err := m.CleanupProjectWithPlan(ctx, "owner", "paper", "project", preview.PlanDigest); err == nil {
		t.Fatal("cleanup deleted a live session")
	}
	if err := m.CloseSession(ctx, "owner", s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetSession(ctx, "owner", s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("closed session remained live")
	}
}

func TestSessionKeepsRunningSnapshotWhileReplacingPending(t *testing.T) {
	m, req := sessionManager(t)
	bin := t.TempDir()
	script := `#!/bin/sh
cat main.tex > main.pdf
printf 'INPUT main.tex\nOUTPUT main.pdf\n' > main.fls
`
	if err := os.WriteFile(filepath.Join(bin, "latexmk"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.SubmitRevision(ctx, "owner", s.ID, planRevision(t, m, req, "first", 0))
	if err != nil {
		t.Fatal(err)
	}
	token := <-m.queue
	running := m.takeSession(token)
	if running != first.ID {
		t.Fatal("wrong running job")
	}
	second, err := m.SubmitRevision(ctx, "owner", s.ID, planRevision(t, m, req, "second", 1))
	if err != nil {
		t.Fatal(err)
	}
	third, err := m.SubmitRevision(ctx, "owner", s.ID, planRevision(t, m, req, "third", 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.queue) != 0 {
		t.Fatal("same session scheduled concurrently")
	}
	m.run(ctx, 1, running)
	first, _ = m.Get(ctx, "owner", first.ID)
	if first.Status != "succeeded" {
		t.Fatalf("first=%+v", first)
	}
	cancelled, _ := m.Get(ctx, "owner", second.ID)
	if cancelled.Status != "cancelled" {
		t.Fatal("intermediate revision ran")
	}
	if got := m.takeSession(<-m.queue); got != third.ID {
		t.Fatal("latest revision was not scheduled")
	} else {
		m.run(ctx, 1, got)
	}
	state, err := m.GetSession(ctx, "owner", s.ID)
	if err != nil || state.LastSuccessfulJobID != third.ID || state.RunningJobID != "" {
		t.Fatalf("session=%+v %v", state, err)
	}
}

func TestSessionExpiryReclaimsPinsAndCancelsPending(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.RealtimeSessionTTL = time.Minute
	s, err := m.CreateSession(context.Background(), "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	// Admission renews the lease, then expiration cancels the pending job.
	m.cfg.RealtimeSessionTTL = time.Hour
	job, err := m.SubmitRevision(context.Background(), "owner", s.ID, planRevision(t, m, req, "source", 0))
	if err != nil {
		t.Fatal(err)
	}
	m.sessions[s.ID].state.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := m.GetSession(context.Background(), "owner", s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
	job, _ = m.Get(context.Background(), "owner", job.ID)
	if job.Status != "cancelled" {
		t.Fatalf("expired job=%+v", job)
	}
}
