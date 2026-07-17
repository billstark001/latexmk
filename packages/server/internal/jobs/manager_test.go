package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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
	request := api.CompileRequest{ProtocolVersion: api.ProtocolVersion, Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"}
	plan, err := projects.Plan("member", api.UploadPlanRequest{ProjectID: "paper", Request: request, Files: []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}}})
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
	manager := New(cfg, api.Metadata{}, compile.NewRunner(cfg), projects, nil, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
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
	request := api.CompileRequest{ProtocolVersion: api.ProtocolVersion, Entry: "main.tex", Engine: "xelatex", Interaction: "nonstopmode"}
	first := commitTestSnapshot(t, projects, request, []byte("first version"))
	manager := New(cfg, api.Metadata{}, compile.NewRunner(cfg), projects, nil, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
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

func commitTestSnapshot(t *testing.T, projects *project.Manager, request api.CompileRequest, content []byte) project.Snapshot {
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
