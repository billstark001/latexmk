// Package jobs runs compile work outside HTTP request lifetimes. The queue is
// bounded, has a fixed worker count, and persists its metadata when a
// PostgreSQL/PGlite store is configured.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

type cleanupResultTarget struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
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
	workers     sync.WaitGroup
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
		m.workers.Add(1)
		go m.worker(ctx, i+1)
	}
	go m.pruneLoop(ctx)
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

// CleanupProject returns a preview. Destructive cleanup is only exposed via
// CleanupProjectWithPlan so callers cannot bypass the preview/digest contract.
func (m *Manager) CleanupProject(ctx context.Context, ownerID, projectID, scope string) (api.CleanupReport, error) {
	return m.cleanupProject(ctx, ownerID, projectID, scope, true, "")
}

func (m *Manager) CleanupProjectWithPlan(ctx context.Context, ownerID, projectID, scope, expectedDigest string) (api.CleanupReport, error) {
	if expectedDigest == "" {
		return api.CleanupReport{}, errors.New("cleanup plan digest is required")
	}
	return m.cleanupProject(ctx, ownerID, projectID, scope, false, expectedDigest)
}

func (m *Manager) cleanupProject(ctx context.Context, ownerID, projectID, scope string, dryRun bool, expectedDigest string) (api.CleanupReport, error) {
	report := api.CleanupReport{ProjectID: projectID, Scope: scope, DryRun: dryRun}
	if !project.ValidProjectID(projectID) {
		return report, errors.New("project ID is invalid")
	}
	if scope != "results" && scope != "snapshot" && scope != "project" {
		return report, errors.New("cleanup scope must be results, snapshot, or project")
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	records, err := m.projectRecords(ctx, ownerID, projectID)
	if err != nil {
		return report, err
	}
	terminalIDs := make([]string, 0, len(records))
	var resultTargets []cleanupResultTarget
	for _, rec := range records {
		switch rec.Job.Status {
		case "queued", "running":
			report.ActiveJobs = append(report.ActiveJobs, rec.Job.ID)
		case "succeeded", "failed", "cancelled":
			terminalIDs = append(terminalIDs, rec.Job.ID)
			if scope == "results" || scope == "project" {
				exists, size, infoErr := m.projects.ResultInfo(ownerID, rec.Job.ID)
				if infoErr != nil {
					return report, infoErr
				}
				if exists {
					report.Results++
					report.ResultBytes += size
					resultTargets = append(resultTargets, cleanupResultTarget{ID: rec.Job.ID, Size: size})
				}
			}
		}
	}
	if scope == "project" {
		report.Jobs = len(terminalIDs)
	}
	snapshotID := ""
	if scope == "snapshot" || scope == "project" {
		report.SnapshotPresent, report.SnapshotFiles, report.SnapshotBytes, err = m.projects.SnapshotStats(ctx, ownerID, projectID)
		if err != nil {
			return report, err
		}
		if len(report.ActiveJobs) > 0 && !dryRun {
			return report, errors.New("project has active jobs; wait for them to finish or cancel queued jobs")
		}
		if report.SnapshotPresent {
			snapshot, snapshotErr := m.projects.Snapshot(ctx, ownerID, projectID)
			if snapshotErr != nil {
				return report, snapshotErr
			}
			snapshotID = snapshot.ID
		}
	}
	sort.Strings(terminalIDs)
	sort.Slice(resultTargets, func(i, j int) bool { return resultTargets[i].ID < resultTargets[j].ID })
	digest, err := cleanupReportDigest(report, terminalIDs, resultTargets, snapshotID)
	if err != nil {
		return report, err
	}
	report.PlanDigest = digest
	if dryRun {
		return report, nil
	}
	if expectedDigest != digest {
		return report, errors.New("cleanup targets changed since preview; create a new plan")
	}
	if scope == "results" || scope == "project" {
		for _, id := range terminalIDs {
			reclaimed, deleteErr := m.projects.DeleteResult(ownerID, id)
			if deleteErr != nil {
				return report, deleteErr
			}
			report.ReclaimedBytes += reclaimed
		}
	}
	if scope == "project" {
		if err := m.deleteTerminalProjectRecords(ctx, ownerID, projectID); err != nil {
			return report, err
		}
	}
	if scope == "snapshot" || scope == "project" {
		if _, err := m.projects.DeleteSnapshot(ctx, ownerID, projectID); err != nil {
			return report, err
		}
		reclaimed, err := m.projects.CollectUnreferencedBlobs(ctx)
		if err != nil {
			return report, err
		}
		report.ReclaimedBytes += reclaimed
	}
	return report, nil
}

func cleanupReportDigest(report api.CleanupReport, terminalIDs []string, resultTargets []cleanupResultTarget, snapshotID string) (string, error) {
	report.DryRun = false
	report.PlanDigest = ""
	report.ReclaimedBytes = 0
	report.ActiveJobs = append([]string(nil), report.ActiveJobs...)
	sort.Strings(report.ActiveJobs)
	targets := struct {
		Report      api.CleanupReport     `json:"report"`
		TerminalIDs []string              `json:"terminalJobIds,omitempty"`
		Results     []cleanupResultTarget `json:"results,omitempty"`
		SnapshotID  string                `json:"snapshotId,omitempty"`
	}{Report: report, Results: resultTargets, SnapshotID: snapshotID}
	if report.Scope == "project" {
		targets.TerminalIDs = append([]string(nil), terminalIDs...)
	}
	payload, err := json.Marshal(targets)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func (m *Manager) projectRecords(ctx context.Context, ownerID, projectID string) ([]record, error) {
	if m.db != nil {
		rows, err := m.db.ListProjectJobs(ctx, ownerID, projectID)
		if err != nil {
			return nil, err
		}
		out := make([]record, 0, len(rows))
		for _, row := range rows {
			out = append(out, record{OwnerID: row.OwnerID, Job: api.Job{ID: row.ID, ProjectID: row.ProjectID, Status: row.Status}})
		}
		return out, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []record
	for _, rec := range m.jobs {
		if rec.OwnerID == ownerID && rec.Job.ProjectID == projectID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (m *Manager) deleteTerminalProjectRecords(ctx context.Context, ownerID, projectID string) error {
	if m.db != nil {
		return m.db.DeleteTerminalProjectJobs(ctx, ownerID, projectID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.jobs {
		if rec.OwnerID == ownerID && rec.Job.ProjectID == projectID && isTerminal(rec.Job.Status) {
			delete(m.jobs, id)
		}
	}
	return nil
}

func isTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}

func (m *Manager) worker(ctx context.Context, worker int) {
	defer m.workers.Done()
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
		if !errors.Is(err, store.ErrJobNotFound) {
			m.requeue(ctx, id)
		}
		return
	}
	if rec.Job.Status != "queued" {
		return
	}
	now := time.Now().UTC()
	rec.Job.Status, rec.Job.StartedAt = "running", &now
	changed, err := m.transition(ctx, rec, "queued")
	if err != nil {
		m.logger.Error("mark job running", "job_id", id, "error", err)
		m.requeue(ctx, id)
		return
	}
	if !changed {
		return
	}
	m.logger.Info("compile job started", "job_id", id, "worker", worker, "owner_id", rec.OwnerID)

	root, err := os.MkdirTemp(m.cfg.TempDir, "latexmk-job-*")
	if err != nil {
		m.finish(ctx, rec, nil, "could not create compile workspace", false)
		return
	}
	defer os.RemoveAll(root)
	workspace := filepath.Join(root, "project")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		m.finish(ctx, rec, nil, "could not initialize compile workspace", false)
		return
	}
	if err := m.projects.Materialize(rec.Snapshot, workspace); err != nil {
		m.finish(ctx, rec, nil, "could not materialize project: "+err.Error(), false)
		return
	}
	output := m.runner.Run(ctx, workspace, rec.Request, rec.Job.ID)
	output.Result.ServerVersion = m.meta.Version
	output.Result.ImageProfile = m.meta.ImageProfile
	_, err = m.projects.WriteResult(rec.OwnerID, rec.Job.ID, output)
	if err != nil {
		m.finish(ctx, rec, &output.Result, "could not package compile result: "+err.Error(), false)
		return
	}
	m.finish(ctx, rec, &output.Result, output.Result.Error, true)
}

func (m *Manager) finish(ctx context.Context, rec record, result *api.CompileResult, message string, resultArchived bool) {
	now := time.Now().UTC()
	if !resultArchived && result != nil && result.Success {
		failed := *result
		failed.Success = false
		if failed.Error == "" {
			failed.Error = message
		}
		result = &failed
	}
	rec.Job.FinishedAt = &now
	rec.Job.Result = result
	rec.Job.Error = message
	if resultArchived && result != nil && result.Success {
		rec.Job.Status = "succeeded"
	} else {
		rec.Job.Status = "failed"
	}
	persistCtx, cancel := m.persistenceContext(ctx)
	defer cancel()
	changed, err := m.transitionWithRetry(persistCtx, rec, "running")
	if err != nil {
		m.logger.Error("finish compile job", "job_id", rec.Job.ID, "error", err)
		return
	}
	if !changed {
		m.logger.Warn("compile job state changed before finish", "job_id", rec.Job.ID)
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
	changed, err := m.transition(ctx, rec, "queued")
	if err != nil {
		return err
	}
	if !changed {
		return errors.New("job is no longer queued")
	}
	m.projects.ReleaseSnapshot(rec.Snapshot.ID)
	return nil
}

func (m *Manager) transition(ctx context.Context, rec record, expectedStatus string) (bool, error) {
	if m.db == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		current, ok := m.jobs[rec.Job.ID]
		if !ok {
			return false, errors.New("job not found")
		}
		if current.Job.Status != expectedStatus {
			return false, nil
		}
		current.Job = rec.Job
		m.jobs[rec.Job.ID] = current
		return true, nil
	}
	result, err := marshalResult(rec.Job.Result)
	if err != nil {
		return false, err
	}
	return m.db.TransitionJob(ctx, rec.Job.ID, expectedStatus, map[string]any{
		"status": rec.Job.Status, "result": result, "error": rec.Job.Error,
		"started_at": rec.Job.StartedAt, "finished_at": rec.Job.FinishedAt,
	})
}

func (m *Manager) transitionWithRetry(ctx context.Context, rec record, expectedStatus string) (bool, error) {
	for attempt := 0; ; attempt++ {
		changed, err := m.transition(ctx, rec, expectedStatus)
		if err == nil {
			return changed, nil
		}
		delay := time.Duration(1<<min(attempt, 5)) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func (m *Manager) requeue(ctx context.Context, id string) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	select {
	case <-ctx.Done():
	case m.queue <- id:
	}
}

func (m *Manager) persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.Background(), m.cfg.ShutdownTimeout)
}

func (m *Manager) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) pruneLoop(ctx context.Context) {
	if m.cfg.StateSweepInterval <= 0 || m.cfg.ResultRetention <= 0 {
		return
	}
	m.pruneTerminal(ctx, time.Now().UTC().Add(-m.cfg.ResultRetention))
	ticker := time.NewTicker(m.cfg.StateSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.pruneTerminal(ctx, now.UTC().Add(-m.cfg.ResultRetention))
		}
	}
}

func (m *Manager) pruneTerminal(ctx context.Context, cutoff time.Time) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	if m.db != nil {
		removed, err := m.db.DeleteTerminalJobsBefore(ctx, cutoff)
		if err != nil {
			m.logger.Error("terminal job metadata sweep failed", "error", err)
		} else if removed > 0 {
			m.logger.Info("terminal job metadata swept", "jobs", removed)
		}
		return
	}
	m.mu.Lock()
	removed := 0
	for id, rec := range m.jobs {
		terminal := rec.Job.Status == "succeeded" || rec.Job.Status == "failed" || rec.Job.Status == "cancelled"
		if terminal && rec.Job.FinishedAt != nil && rec.Job.FinishedAt.Before(cutoff) {
			delete(m.jobs, id)
			removed++
		}
	}
	m.mu.Unlock()
	if removed > 0 {
		m.logger.Info("terminal job metadata swept", "jobs", removed)
	}
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
		defer m.mu.Unlock()
		if _, exists := m.jobs[rec.Job.ID]; exists {
			return errors.New("job already exists")
		}
		m.jobs[rec.Job.ID] = rec
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
	result, err := marshalResult(rec.Job.Result)
	if err != nil {
		return err
	}
	return m.db.CreateJob(ctx, store.CompileJob{ID: rec.Job.ID, OwnerID: rec.OwnerID, ProjectID: rec.Job.ProjectID, SnapshotID: rec.Snapshot.ID, SnapshotManifest: snapshot, Status: rec.Job.Status, Request: request, Result: result, Error: rec.Job.Error, CreatedAt: rec.Job.CreatedAt, StartedAt: rec.Job.StartedAt, FinishedAt: rec.Job.FinishedAt})
}

func marshalResult(result *api.CompileResult) ([]byte, error) {
	if result == nil {
		return nil, nil
	}
	return json.Marshal(result)
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
