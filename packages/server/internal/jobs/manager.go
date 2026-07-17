// Package jobs runs compile work outside HTTP request lifetimes. The queue is
// bounded, has a fixed worker count, and persists its metadata when a
// PostgreSQL/PGlite store is configured.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/store"
)

type record struct {
	Job      api.Job
	OwnerID  string
	Request  api.CompileRequest
	Snapshot project.Snapshot
}

type Manager struct {
	cfg      config.Config
	meta     api.Metadata
	runner   *compile.Runner
	projects *project.Manager
	db       *store.Postgres
	logger   *slog.Logger

	mu          sync.Mutex
	admissionMu sync.Mutex
	jobs        map[string]record
	queue       chan string
	ctx         context.Context
}

func New(cfg config.Config, meta api.Metadata, runner *compile.Runner, projects *project.Manager, db *store.Postgres, logger *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, meta: meta, runner: runner, projects: projects, db: db, logger: logger,
		// Cancellation is cooperative: a cancelled identifier can still be in
		// the channel until a worker observes it. Extra channel room prevents a
		// burst of cancellations from blocking an otherwise valid replacement.
		jobs: make(map[string]record), queue: make(chan string, cfg.MaxQueuedJobs*2),
	}
}

func (m *Manager) Start(ctx context.Context) {
	m.ctx = ctx
	recoverIDs := make([]string, 0)
	if m.db != nil {
		pending, err := m.db.ListPendingJobs(ctx)
		if err != nil {
			m.logger.Error("could not recover queued jobs", "error", err)
		} else {
			for _, job := range pending {
				rec, decodeErr := recordFromRow(job)
				if decodeErr != nil {
					now := time.Now().UTC()
					_ = m.db.UpdateJob(ctx, job.ID, map[string]any{"status": "failed", "error": "queued job has no valid immutable snapshot; submit it again", "finished_at": &now})
					m.logger.Warn("discarded queued job without immutable snapshot", "job_id", job.ID, "error", decodeErr)
					continue
				}
				if err := m.projects.PinSnapshot(rec.Snapshot); err != nil {
					now := time.Now().UTC()
					_ = m.db.UpdateJob(ctx, job.ID, map[string]any{"status": "failed", "error": "queued job snapshot is invalid; submit it again", "finished_at": &now})
					m.logger.Warn("discarded queued job with invalid snapshot", "job_id", job.ID, "error", err)
					continue
				}
				// A crash may leave a job marked running. It is safe to retry:
				// every execution gets a new isolated workspace and archive.
				if job.Status == "running" {
					if err := m.db.UpdateJob(ctx, job.ID, map[string]any{"status": "queued", "started_at": nil}); err != nil {
						m.projects.ReleaseSnapshot(rec.Snapshot.ID)
						m.logger.Error("could not reset running job for recovery", "job_id", job.ID, "error", err)
						continue
					}
				}
				recoverIDs = append(recoverIDs, job.ID)
			}
		}
	}
	for i := 0; i < m.cfg.MaxConcurrentCompiles; i++ {
		go m.worker(ctx, i+1)
	}
	go func() {
		for _, id := range recoverIDs {
			select {
			case <-ctx.Done():
				return
			case m.queue <- id:
			}
		}
	}()
}

func (m *Manager) Enqueue(ctx context.Context, ownerID string, snapshot project.Snapshot, request api.CompileRequest) (api.Job, error) {
	if err := m.runner.ValidateRequest(request); err != nil {
		return api.Job{}, err
	}
	if snapshot.OwnerID != ownerID {
		return api.Job{}, errors.New("snapshot owner does not match authenticated owner")
	}
	if err := m.projects.PinSnapshot(snapshot); err != nil {
		return api.Job{}, fmt.Errorf("pin project snapshot: %w", err)
	}
	pinned := true
	defer func() {
		if pinned {
			m.projects.ReleaseSnapshot(snapshot.ID)
		}
	}()
	// Counting, persisting, and publishing a queued job must be one admission
	// operation. Without this guard a burst can observe the same remaining slot
	// and exceed MaxQueuedJobs before any worker gets a chance to run.
	m.admissionMu.Lock()
	pending, err := m.pendingCount(ctx)
	if err != nil {
		m.admissionMu.Unlock()
		return api.Job{}, err
	}
	if pending >= m.cfg.MaxQueuedJobs {
		m.admissionMu.Unlock()
		return api.Job{}, errors.New("compile queue is full")
	}
	id, err := randomID("job")
	if err != nil {
		m.admissionMu.Unlock()
		return api.Job{}, err
	}
	now := time.Now().UTC()
	rec := record{Job: api.Job{ID: id, ProjectID: snapshot.ProjectID, SnapshotID: snapshot.ID, Status: "queued", CreatedAt: now}, OwnerID: ownerID, Request: request, Snapshot: snapshot}
	if err := m.save(ctx, rec); err != nil {
		m.admissionMu.Unlock()
		return api.Job{}, err
	}
	select {
	case m.queue <- id:
		pinned = false
		m.admissionMu.Unlock()
		return rec.Job, nil
	default:
		m.admissionMu.Unlock()
		// Do not retain a row which cannot ever be scheduled.
		pinned = false
		_ = m.cancel(ctx, id, "compile queue is full")
		return api.Job{}, errors.New("compile queue is full")
	}
}

func (m *Manager) pendingCount(ctx context.Context) (int, error) {
	if m.db == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		count := 0
		for _, rec := range m.jobs {
			if rec.Job.Status == "queued" {
				count++
			}
		}
		return count, nil
	}
	rows, err := m.db.ListPendingJobs(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		if row.Status == "queued" {
			count++
		}
	}
	return count, nil
}

func (m *Manager) Get(ctx context.Context, ownerID, id string) (api.Job, error) {
	rec, err := m.load(ctx, id)
	if err != nil {
		return api.Job{}, err
	}
	if rec.OwnerID != ownerID {
		return api.Job{}, errors.New("job not found")
	}
	return rec.Job, nil
}

func (m *Manager) List(ctx context.Context, ownerID string, limit int) ([]api.Job, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if m.db == nil {
		m.mu.Lock()
		out := make([]api.Job, 0, len(m.jobs))
		for _, rec := range m.jobs {
			if rec.OwnerID == ownerID {
				out = append(out, rec.Job)
			}
		}
		m.mu.Unlock()
		// The in-memory order is not stable, but timestamps make it deterministic
		// after sorting in the HTTP layer unnecessary for the common small queue.
		if len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	}
	rows, err := m.db.ListJobs(ctx, ownerID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.Job, 0, len(rows))
	for _, row := range rows {
		rec, err := recordFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec.Job)
	}
	return out, nil
}

func (m *Manager) Cancel(ctx context.Context, ownerID, id string) (api.Job, error) {
	rec, err := m.load(ctx, id)
	if err != nil {
		return api.Job{}, err
	}
	if rec.OwnerID != ownerID {
		return api.Job{}, errors.New("job not found")
	}
	if rec.Job.Status != "queued" {
		return api.Job{}, errors.New("only queued jobs can be cancelled")
	}
	if err := m.cancel(ctx, id, "cancelled by user"); err != nil {
		return api.Job{}, err
	}
	return m.Get(ctx, ownerID, id)
}

func (m *Manager) ResultPath(ctx context.Context, ownerID, id string) (string, api.Job, error) {
	job, err := m.Get(ctx, ownerID, id)
	if err != nil {
		return "", api.Job{}, err
	}
	if job.Status != "succeeded" && job.Status != "failed" {
		return "", job, errors.New("job result is not ready")
	}
	path, err := m.projects.ResultPath(ownerID, id)
	if err != nil {
		return "", job, err
	}
	if _, err := os.Stat(path); err != nil {
		return "", job, errors.New("job result archive is unavailable")
	}
	return path, job, nil
}

func (m *Manager) worker(ctx context.Context, worker int) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			m.run(ctx, worker, id)
		}
	}
}

func (m *Manager) run(ctx context.Context, worker int, id string) {
	rec, err := m.load(ctx, id)
	if err != nil {
		m.logger.Error("load queued job", "job_id", id, "error", err)
		return
	}
	if rec.Job.Status != "queued" {
		return
	}
	now := time.Now().UTC()
	rec.Job.Status, rec.Job.StartedAt = "running", &now
	if err := m.save(ctx, rec); err != nil {
		m.logger.Error("mark job running", "job_id", id, "error", err)
		return
	}
	m.logger.Info("compile job started", "job_id", id, "worker", worker, "owner_id", rec.OwnerID)

	root, err := os.MkdirTemp(m.cfg.TempDir, "latexmk-job-*")
	if err != nil {
		m.finish(ctx, rec, nil, "could not create compile workspace")
		return
	}
	defer os.RemoveAll(root)
	workspace := filepath.Join(root, "project")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		m.finish(ctx, rec, nil, "could not initialize compile workspace")
		return
	}
	if err := m.projects.Materialize(rec.Snapshot, workspace); err != nil {
		m.finish(ctx, rec, nil, "could not materialize project: "+err.Error())
		return
	}
	output := m.runner.Run(ctx, workspace, rec.Request, rec.Job.ID)
	output.Result.ServerVersion = m.meta.Version
	output.Result.ImageProfile = m.meta.ImageProfile
	_, err = m.projects.WriteResult(rec.OwnerID, rec.Job.ID, output)
	if err != nil {
		m.finish(ctx, rec, &output.Result, "could not package compile result: "+err.Error())
		return
	}
	m.finish(ctx, rec, &output.Result, output.Result.Error)
}

func (m *Manager) finish(ctx context.Context, rec record, result *api.CompileResult, message string) {
	now := time.Now().UTC()
	rec.Job.FinishedAt = &now
	rec.Job.Result = result
	rec.Job.Error = message
	if result != nil && result.Success {
		rec.Job.Status = "succeeded"
	} else {
		rec.Job.Status = "failed"
	}
	if err := m.save(ctx, rec); err != nil {
		m.logger.Error("finish compile job", "job_id", rec.Job.ID, "error", err)
		return
	}
	m.projects.ReleaseSnapshot(rec.Snapshot.ID)
	m.logger.Info("compile job finished", "job_id", rec.Job.ID, "status", rec.Job.Status, "duration_ms", resultDuration(result))
}

func (m *Manager) cancel(ctx context.Context, id, message string) error {
	rec, err := m.load(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	rec.Job.Status, rec.Job.Error, rec.Job.FinishedAt = "cancelled", message, &now
	if err := m.save(ctx, rec); err != nil {
		return err
	}
	m.projects.ReleaseSnapshot(rec.Snapshot.ID)
	return nil
}

func (m *Manager) load(ctx context.Context, id string) (record, error) {
	if m.db == nil {
		m.mu.Lock()
		rec, ok := m.jobs[id]
		m.mu.Unlock()
		if !ok {
			return record{}, errors.New("job not found")
		}
		return rec, nil
	}
	row, err := m.db.GetJob(ctx, id)
	if err != nil {
		return record{}, err
	}
	return recordFromRow(row)
}

func (m *Manager) save(ctx context.Context, rec record) error {
	if m.db == nil {
		m.mu.Lock()
		m.jobs[rec.Job.ID] = rec
		m.mu.Unlock()
		return nil
	}
	request, err := json.Marshal(rec.Request)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(rec.Snapshot)
	if err != nil {
		return err
	}
	var result []byte
	if rec.Job.Result != nil {
		result, err = json.Marshal(rec.Job.Result)
		if err != nil {
			return err
		}
	}
	if _, err := m.db.GetJob(ctx, rec.Job.ID); err != nil {
		return m.db.CreateJob(ctx, store.CompileJob{ID: rec.Job.ID, OwnerID: rec.OwnerID, ProjectID: rec.Job.ProjectID, SnapshotID: rec.Snapshot.ID, SnapshotManifest: snapshot, Status: rec.Job.Status, Request: request, Result: result, Error: rec.Job.Error, CreatedAt: rec.Job.CreatedAt, StartedAt: rec.Job.StartedAt, FinishedAt: rec.Job.FinishedAt})
	}
	return m.db.UpdateJob(ctx, rec.Job.ID, map[string]any{"status": rec.Job.Status, "result": result, "error": rec.Job.Error, "started_at": rec.Job.StartedAt, "finished_at": rec.Job.FinishedAt})
}

func recordFromRow(row store.CompileJob) (record, error) {
	var request api.CompileRequest
	if err := json.Unmarshal(row.Request, &request); err != nil {
		return record{}, fmt.Errorf("decode queued job request: %w", err)
	}
	job := api.Job{ID: row.ID, ProjectID: row.ProjectID, Status: row.Status, CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, Error: row.Error}
	if len(row.Result) > 0 {
		var result api.CompileResult
		if err := json.Unmarshal(row.Result, &result); err != nil {
			return record{}, fmt.Errorf("decode queued job result: %w", err)
		}
		job.Result = &result
	}
	if len(row.SnapshotManifest) == 0 {
		if row.Status == "queued" || row.Status == "running" {
			return record{}, errors.New("active job is missing its immutable snapshot")
		}
		return record{Job: job, OwnerID: row.OwnerID, Request: request}, nil
	}
	var snapshot project.Snapshot
	if err := json.Unmarshal(row.SnapshotManifest, &snapshot); err != nil {
		return record{}, fmt.Errorf("decode queued job snapshot: %w", err)
	}
	if err := project.ValidateSnapshot(snapshot); err != nil {
		return record{}, fmt.Errorf("validate queued job snapshot: %w", err)
	}
	if row.SnapshotID != "" && row.SnapshotID != snapshot.ID {
		return record{}, errors.New("queued job snapshot ID does not match its manifest")
	}
	if row.OwnerID != snapshot.OwnerID || row.ProjectID != snapshot.ProjectID {
		return record{}, errors.New("queued job snapshot scope does not match its job")
	}
	job.SnapshotID = snapshot.ID
	return record{Job: job, OwnerID: row.OwnerID, Request: request, Snapshot: snapshot}, nil
}

func resultDuration(result *api.CompileResult) int64 {
	if result == nil {
		return 0
	}
	return result.DurationMS
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}
