package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/store"
)

func TestQueueAcceptsMultipleJobsAndAllowsQueuedCancellation(t *testing.T) {
	cfg := config.Config{
		StateDir: t.TempDir(), Engines: []string{"xelatex"}, MaxFiles: 10,
		MaxExpandedBytes: 1024, MaxConcurrentCompiles: 1, MaxQueuedJobs: 2,
		MaxStateBytes: 1024,
	}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("\\documentclass{article}")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	request := api.CompileRequest{
		ProtocolVersion: api.ProtocolVersion,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Interaction:     "nonstopmode",
	}
	plan, err := projects.Plan(
		"member",
		api.UploadPlanRequest{
			ProjectID: "paper",
			Request:   request,
			Files:     []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.PutBlob("member", plan.UploadID, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := projects.Commit(context.Background(), "member", plan.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(
		cfg,
		api.Metadata{},
		compile.NewRunner(cfg),
		projects,
		nil,
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	first, err := manager.Enqueue(context.Background(), "member", snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Enqueue(context.Background(), "member", snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enqueue(context.Background(), "member", snapshot, request); err == nil {
		t.Fatal("expected bounded queue to reject a third job")
	}
	cancelled, err := manager.Cancel(context.Background(), "member", second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancelled job has status %q", cancelled.Status)
	}
	if got, err := manager.Get(context.Background(), "member", first.ID); err != nil || got.Status != "queued" {
		t.Fatalf("first job = %#v, %v", got, err)
	}
}

func TestQueuedTransitionCannotOverwriteCancellation(t *testing.T) {
	cfg := config.Config{MaxConcurrentCompiles: 1, MaxQueuedJobs: 2}
	manager := &Manager{
		cfg:    cfg,
		jobs:   make(map[string]record),
		logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}
	now := time.Now().UTC()
	original := record{OwnerID: "member", Job: api.Job{ID: "job_race", Status: "queued", CreatedAt: now}}
	manager.jobs[original.Job.ID] = original

	staleWorkerCopy := original
	cancelledCopy := original
	finished := time.Now().UTC()
	cancelledCopy.Job.Status = "cancelled"
	cancelledCopy.Job.Error = "cancelled by user"
	cancelledCopy.Job.FinishedAt = &finished
	changed, err := manager.transition(context.Background(), cancelledCopy, "queued")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected cancellation transition to win")
	}
	started := time.Now().UTC()
	staleWorkerCopy.Job.Status = "running"
	staleWorkerCopy.Job.StartedAt = &started
	changed, err = manager.transition(context.Background(), staleWorkerCopy, "queued")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("stale worker transition overwrote cancellation")
	}
	got, err := manager.load(context.Background(), original.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Job.Status != "cancelled" {
		t.Fatalf("job status = %q, want cancelled", got.Job.Status)
	}
}

func TestSuccessfulCompileRequiresArchivedResult(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), MaxStateBytes: 4096, ShutdownTimeout: time.Second}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(
		cfg,
		api.Metadata{},
		compile.NewRunner(config.Config{MaxConcurrentCompiles: 1}),
		projects,
		nil,
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	now := time.Now().UTC()
	rec := record{OwnerID: "member", Job: api.Job{ID: "job_archive_failed", Status: "running", CreatedAt: now}}
	if err := manager.save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	result := &api.CompileResult{Success: true, ExitCode: 0}
	manager.finish(context.Background(), rec, result, "could not package compile result", false)
	got, err := manager.Get(context.Background(), "member", rec.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error != "could not package compile result" || got.Result == nil ||
		got.Result.Success {
		t.Fatalf("job = %#v, want failed packaging status", got)
	}
}

func TestQueuedJobKeepsSnapshotCapturedAtEnqueue(t *testing.T) {
	cfg := config.Config{
		StateDir: t.TempDir(), Engines: []string{"xelatex"}, MaxFiles: 10,
		MaxUploadBytes: 1024, MaxExpandedBytes: 1024, MaxConcurrentCompiles: 1,
		MaxQueuedJobs: 2, MaxStateBytes: 4096,
	}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := api.CompileRequest{
		ProtocolVersion: api.ProtocolVersion,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Interaction:     "nonstopmode",
	}
	first := commitTestSnapshot(t, projects, request, []byte("first version"))
	manager := New(
		cfg,
		api.Metadata{},
		compile.NewRunner(cfg),
		projects,
		nil,
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	job, err := manager.Enqueue(context.Background(), "member", first, request)
	if err != nil {
		t.Fatal(err)
	}
	second := commitTestSnapshot(t, projects, request, []byte("second version"))
	if first.ID == second.ID {
		t.Fatal("different manifests received the same snapshot ID")
	}

	rec, err := manager.load(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Snapshot.ID != first.ID || rec.Job.SnapshotID != first.ID {
		t.Fatalf("queued snapshot = %q/%q, want %q", rec.Snapshot.ID, rec.Job.SnapshotID, first.ID)
	}
	workspace := t.TempDir()
	if err := projects.Materialize(rec.Snapshot, workspace); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first version" {
		t.Fatalf("queued job materialized %q, want first version", content)
	}
}

func TestLegacyFinishedJobWithoutSnapshotRemainsReadable(t *testing.T) {
	row := store.CompileJob{
		ID: "job_legacy", OwnerID: "member", ProjectID: "paper", Status: "succeeded",
		Request: []byte(`{"protocolVersion":2,"entry":"main.tex","engine":"xelatex"}`),
		Result:  []byte(`{"protocolVersion":2,"requestId":"job_legacy","success":true,"exitCode":0}`),
	}
	rec, err := recordFromRow(row)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Job.Status != "succeeded" || rec.Job.Result == nil || !rec.Job.Result.Success {
		t.Fatalf("legacy job was not decoded: %#v", rec.Job)
	}
	row.Status = "queued"
	if _, err := recordFromRow(row); err == nil {
		t.Fatal("expected active legacy job without snapshot to be rejected")
	}
}

func TestCleanupPlanRejectsChangedTargets(t *testing.T) {
	cfg := config.Config{
		StateDir:              t.TempDir(),
		Engines:               []string{"xelatex"},
		MaxFiles:              10,
		MaxUploadBytes:        1024,
		MaxExpandedBytes:      1024,
		MaxConcurrentCompiles: 1,
		MaxQueuedJobs:         4,
		MaxStateBytes:         4096,
	}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := api.CompileRequest{
		ProtocolVersion: api.ProtocolVersion,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Interaction:     "nonstopmode",
	}
	commitTestSnapshot(t, projects, request, []byte("first version"))
	manager := New(
		cfg,
		api.Metadata{},
		compile.NewRunner(cfg),
		projects,
		nil,
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	now := time.Now().UTC()
	manager.jobs["job_first"] = record{
		OwnerID: "member",
		Job:     api.Job{ID: "job_first", ProjectID: "paper", Status: "cancelled", CreatedAt: now, FinishedAt: &now},
	}
	preview, err := manager.CleanupProject(context.Background(), "member", "paper", "project")
	if err != nil {
		t.Fatal(err)
	}
	manager.jobs["job_second"] = record{
		OwnerID: "member",
		Job:     api.Job{ID: "job_second", ProjectID: "paper", Status: "cancelled", CreatedAt: now, FinishedAt: &now},
	}
	if _, err := manager.CleanupProjectWithPlan(
		context.Background(),
		"member",
		"paper",
		"project",
		preview.PlanDigest,
	); err == nil {
		t.Fatal("expected a changed target set to invalidate the cleanup plan")
	}
	if _, err := projects.Snapshot(context.Background(), "member", "paper"); err != nil {
		t.Fatalf("stale plan removed snapshot: %v", err)
	}
}

func TestCleanupProjectBlocksActiveJobsAndAppliesExactPreview(t *testing.T) {
	cfg := config.Config{
		StateDir:              t.TempDir(),
		Engines:               []string{"xelatex"},
		MaxFiles:              10,
		MaxUploadBytes:        1024,
		MaxExpandedBytes:      1024,
		MaxConcurrentCompiles: 1,
		MaxQueuedJobs:         4,
		MaxStateBytes:         4096,
	}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := api.CompileRequest{
		ProtocolVersion: api.ProtocolVersion,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Interaction:     "nonstopmode",
	}
	commitTestSnapshot(t, projects, request, []byte("source"))
	manager := New(
		cfg,
		api.Metadata{},
		compile.NewRunner(cfg),
		projects,
		nil,
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	now := time.Now().UTC()
	manager.jobs["job_active"] = record{
		OwnerID: "member",
		Job:     api.Job{ID: "job_active", ProjectID: "paper", Status: "queued", CreatedAt: now},
	}
	preview, err := manager.CleanupProject(context.Background(), "member", "paper", "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.ActiveJobs) != 1 {
		t.Fatalf("active jobs = %v", preview.ActiveJobs)
	}
	if _, err := manager.CleanupProjectWithPlan(
		context.Background(),
		"member",
		"paper",
		"project",
		preview.PlanDigest,
	); err == nil {
		t.Fatal("expected active job to block project cleanup")
	}
	finished := time.Now().UTC()
	rec := manager.jobs["job_active"]
	rec.Job.Status = "cancelled"
	rec.Job.FinishedAt = &finished
	manager.jobs["job_active"] = rec
	preview, err = manager.CleanupProject(context.Background(), "member", "paper", "project")
	if err != nil {
		t.Fatal(err)
	}
	report, err := manager.CleanupProjectWithPlan(
		context.Background(),
		"member",
		"paper",
		"project",
		preview.PlanDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.DryRun || report.PlanDigest != preview.PlanDigest {
		t.Fatalf("unexpected cleanup report: %#v", report)
	}
	if _, err := projects.Snapshot(
		context.Background(),
		"member",
		"paper",
	); !errors.Is(
		err,
		store.ErrProjectSnapshotNotFound,
	) {
		t.Fatalf("snapshot still present: %v", err)
	}
	if _, err := manager.Get(context.Background(), "member", "job_active"); err == nil {
		t.Fatal("terminal job metadata still present")
	}
}

func commitTestSnapshot(
	t *testing.T,
	projects *project.Manager,
	request api.CompileRequest,
	content []byte,
) project.Snapshot {
	t.Helper()
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	plan, err := projects.Plan("member", api.UploadPlanRequest{
		ProjectID: "paper",
		Request:   request,
		Files:     []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Missing) > 0 {
		if err := projects.PutBlob("member", plan.UploadID, sha, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _, err := projects.Commit(context.Background(), "member", plan.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { return len(p), nil }
