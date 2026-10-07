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
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/sandbox"
	"github.com/billstark001/latexmk/packages/server/internal/store"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type record struct {
	InvalidateCheckpoint bool
	CompletionFrom       string
	Job                  protocol.Job
	OwnerID              string
	Request              protocol.CompileRequest
	Snapshot             project.Snapshot
}

type cleanupResultTarget struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// jobStore isolates durable metadata operations from queue/session coordination.
type jobStore interface {
	CountQueuedJobs(context.Context) (int64, error)
	ListPendingJobs(context.Context) ([]store.CompileJob, error)
	UpdateJob(context.Context, string, map[string]any) error
	ListJobs(context.Context, string, int) ([]store.CompileJob, error)
	ListProjectJobs(context.Context, string, string) ([]store.CompileJob, error)
	DeleteTerminalProjectJobs(context.Context, string, string) error
	TransitionJob(context.Context, string, string, map[string]any) (bool, error)
	DeleteTerminalJobsBefore(context.Context, time.Time, []string) (int64, error)
	GetJob(context.Context, string) (store.CompileJob, error)
	CreateJob(context.Context, store.CompileJob) error
}

type Manager struct {
	cfg      config.Config
	meta     protocol.Metadata
	runner   *compile.Runner
	projects *project.Manager
	db       jobStore
	logger   *slog.Logger

	mu                 sync.Mutex
	admissionMu        sync.Mutex
	jobs               map[string]record
	jobHistory         map[string][]string
	queued             int
	completions        map[string]record
	publications       map[string]chan struct{}
	publicationVersion uint64
	queue              chan string
	workers            sync.WaitGroup
	sessions           map[string]*liveSession
	revisionBudgets    map[string]*revisionBudget
	active             map[string]context.CancelFunc
}

func New(
	cfg config.Config,
	meta protocol.Metadata,
	runner *compile.Runner,
	projects *project.Manager,
	db *store.Postgres,
	logger *slog.Logger,
) *Manager {
	var persistence jobStore
	if db != nil {
		persistence = db
	}
	return &Manager{
		cfg:      cfg,
		meta:     meta,
		runner:   runner,
		projects: projects,
		db:       persistence,
		logger:   logger,
		// Cancellation is cooperative: a cancelled identifier can still be in
		// the channel until a worker observes it. Extra channel room prevents a
		// burst of cancellations from blocking an otherwise valid replacement.
		revisionBudgets: make(
			map[string]*revisionBudget,
		),
		sessions:     make(map[string]*liveSession),
		active:       make(map[string]context.CancelFunc),
		jobs:         make(map[string]record),
		jobHistory:   make(map[string][]string),
		completions:  make(map[string]record),
		publications: make(map[string]chan struct{}),
		queue:        make(chan string, cfg.MaxQueuedJobs*2),
	}
}

func (m *Manager) Start(ctx context.Context) {
	m.workers.Add(1)
	go func() { defer m.workers.Done(); m.recoverCompletions(ctx) }()
	m.workers.Add(1)
	go func() { defer m.workers.Done(); m.maintainSessions(ctx) }()
	recoverIDs := make([]string, 0)
	if m.db != nil {
		pending, err := m.db.ListPendingJobs(ctx)
		if err != nil {
			m.logger.Error("could not recover queued jobs", "error", err)
		} else {
			for _, job := range pending {
				if job.SessionID != "" {
					now := time.Now().UTC()
					_ = m.db.UpdateJob(
						ctx,
						job.ID,
						map[string]any{
							"snapshot_manifest": nil,
							"status":            "cancelled",
							"error":             "session ended when the server restarted; submit a new session",
							"finished_at":       &now,
						},
					)
					continue
				}
				rec, decodeErr := recordFromRow(job)
				if decodeErr != nil {
					now := time.Now().UTC()
					_ = m.db.UpdateJob(
						ctx,
						job.ID,
						map[string]any{
							"status":      "failed",
							"error":       "queued job has no valid immutable snapshot; submit it again",
							"finished_at": &now,
						},
					)
					m.logger.Warn(
						"discarded queued job without immutable snapshot",
						"job_id",
						job.ID,
						"error",
						decodeErr,
					)
					continue
				}
				if err := m.projects.PinSnapshot(rec.Snapshot); err != nil {
					now := time.Now().UTC()
					_ = m.db.UpdateJob(
						ctx,
						job.ID,
						map[string]any{
							"status":      "failed",
							"error":       "queued job snapshot is invalid; submit it again",
							"finished_at": &now,
						},
					)
					m.logger.Warn("discarded queued job with invalid snapshot", "job_id", job.ID, "error", err)
					continue
				}
				// A crash may leave a job marked running. It is safe to retry:
				// every execution gets a new isolated workspace and archive.
				if job.Status == "running" {
					if err := m.db.UpdateJob(
						ctx,
						job.ID,
						map[string]any{"status": "queued", "started_at": nil},
					); err != nil {
						m.projects.ReleaseSnapshot(rec.Snapshot.ID)
						m.logger.Error("could not reset running job for recovery", "job_id", job.ID, "error", err)
						continue
					}
				}
				recoverIDs = append(recoverIDs, job.ID)
				m.beginPublication(job.ID)
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

func (m *Manager) Enqueue(
	ctx context.Context,
	ownerID string,
	snapshot project.Snapshot,
	request protocol.CompileRequest,
) (protocol.Job, error) {
	if m.cfg.RunnerImage != "" {
		if err := sandbox.ValidateSourcePaths(snapshot.Files); err != nil {
			return protocol.Job{}, err
		}
	}
	if err := m.runner.ValidateRequest(request); err != nil {
		return protocol.Job{}, err
	}
	if snapshot.OwnerID != ownerID {
		return protocol.Job{}, errors.New("snapshot owner does not match authenticated owner")
	}
	if err := m.projects.PinSnapshot(snapshot); err != nil {
		return protocol.Job{}, fmt.Errorf("pin project snapshot: %w", err)
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
		return protocol.Job{}, err
	}
	if pending >= m.cfg.MaxQueuedJobs {
		m.admissionMu.Unlock()
		return protocol.Job{}, errors.New("compile queue is full")
	}
	id, err := randomID("job")
	if err != nil {
		m.admissionMu.Unlock()
		return protocol.Job{}, err
	}
	now := time.Now().UTC()
	rec := record{
		Job: protocol.Job{
			ID:         id,
			ProjectID:  snapshot.ProjectID,
			SnapshotID: snapshot.ID,
			Status:     "queued",
			CreatedAt:  now,
		},
		OwnerID:  ownerID,
		Request:  request,
		Snapshot: snapshot,
	}
	if err := m.save(ctx, rec); err != nil {
		m.admissionMu.Unlock()
		return protocol.Job{}, err
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
		return protocol.Job{}, errors.New("compile queue is full")
	}
}

func (m *Manager) pendingCount(ctx context.Context) (int, error) {
	if m.db == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.queued + len(m.completions), nil
	}
	queued, err := m.db.CountQueuedJobs(ctx)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	count := int(queued) + len(m.completions)
	m.mu.Unlock()
	return count, nil
}

func (m *Manager) Get(ctx context.Context, ownerID, id string) (protocol.Job, error) {
	m.mu.Lock()
	publication := m.publications[id]
	m.mu.Unlock()
	rec, err := m.load(ctx, id)
	if err != nil {
		return protocol.Job{}, err
	}
	if rec.OwnerID != ownerID {
		return protocol.Job{}, errors.New("job not found")
	}
	if rec.Job.Status == "succeeded" && publication != nil {
		select {
		case <-ctx.Done():
			return protocol.Job{}, ctx.Err()
		case <-publication:
		}
		rec, err = m.load(ctx, id)
		if err != nil {
			return protocol.Job{}, err
		}
	}
	return withoutExpiredAuxiliary(rec.Job), nil
}

func (m *Manager) List(ctx context.Context, ownerID string, limit int) ([]protocol.Job, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	m.mu.Lock()
	publications := maps.Clone(m.publications)
	var native []protocol.Job
	if m.db == nil {
		history := m.jobHistory[ownerID]
		native = make([]protocol.Job, 0, min(limit, len(history)))
		for i := len(history) - 1; i >= 0 && len(native) < limit; i-- {
			native = append(native, m.jobs[history[i]].Job)
		}
	}
	m.mu.Unlock()
	reloadSuccessful := false
	reconcile := func(job protocol.Job) (protocol.Job, error) {
		if gate := publications[job.ID]; job.Status == "succeeded" && gate != nil {
			select {
			case <-ctx.Done():
				return protocol.Job{}, ctx.Err()
			case <-gate:
			}
			return m.Get(ctx, ownerID, job.ID)
		}
		if reloadSuccessful && job.Status == "succeeded" {
			return m.Get(ctx, ownerID, job.ID)
		}
		return withoutExpiredAuxiliary(job), nil
	}
	if m.db == nil {
		for i := range native {
			var err error
			native[i], err = reconcile(native[i])
			if err != nil {
				return nil, err
			}
		}
		return native, nil
	}
	var rows []store.CompileJob
	for attempt := 0; ; attempt++ {
		m.mu.Lock()
		version := m.publicationVersion
		m.mu.Unlock()
		var err error
		rows, err = m.db.ListJobs(ctx, ownerID, limit)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		changed := version != m.publicationVersion
		publications = maps.Clone(m.publications)
		m.mu.Unlock()
		if !changed {
			break
		}
		// A publication can start and finish during the SQL query, leaving a
		// stale row with no remaining gate. Retry the page a bounded number of
		// times, then reload successes individually under sustained churn.
		if attempt == 2 {
			reloadSuccessful = true
			break
		}
	}
	out := make([]protocol.Job, 0, len(rows))
	for _, row := range rows {
		rec, err := recordFromRow(row)
		if err != nil {
			return nil, err
		}
		job, err := reconcile(rec.Job)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, nil
}

func (m *Manager) Cancel(ctx context.Context, ownerID, id string) (protocol.Job, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	rec, err := m.load(ctx, id)
	if err != nil {
		return protocol.Job{}, err
	}
	if rec.OwnerID != ownerID {
		return protocol.Job{}, errors.New("job not found")
	}
	if rec.Job.Status == "queued" {
		if err := m.cancel(ctx, id, "cancelled by user"); err != nil {
			return protocol.Job{}, err
		}
	} else if rec.Job.Status == "running" {
		cancel := m.active[id]
		m.mu.Lock()
		_, completing := m.completions[id]
		m.mu.Unlock()
		if cancel == nil && !completing {
			return protocol.Job{}, errors.New("job is not running on this instance")
		}
		now := time.Now().UTC()
		rec.Job.Status, rec.Job.Error, rec.Job.FinishedAt = "cancelled", "cancelled by user", &now
		if changed, err := m.transition(ctx, rec, "running"); err != nil {
			return protocol.Job{}, err
		} else if !changed {
			return protocol.Job{}, errors.New("job already finished")
		}
		if cancel != nil {
			cancel()
		}
	} else {
		return protocol.Job{}, errors.New("only queued or running jobs can be cancelled")
	}
	return m.Get(ctx, ownerID, id)
}

func (m *Manager) ResultPath(ctx context.Context, ownerID, id string) (string, protocol.Job, error) {
	job, err := m.Get(ctx, ownerID, id)
	if err != nil {
		return "", protocol.Job{}, err
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
	if err := m.projects.PruneResultAuxiliary(ownerID, id); err != nil {
		return "", job, err
	}
	return path, job, nil
}

// CleanupProject returns a preview. Destructive cleanup is only exposed via
// CleanupProjectWithPlan so callers cannot bypass the preview/digest contract.
func (m *Manager) CleanupProject(
	ctx context.Context,
	ownerID, projectID, scope string,
) (protocol.CleanupReport, error) {
	return m.cleanupProject(ctx, ownerID, projectID, scope, true, "")
}

func (m *Manager) CleanupProjectWithPlan(
	ctx context.Context,
	ownerID, projectID, scope, expectedDigest string,
) (protocol.CleanupReport, error) {
	if expectedDigest == "" {
		return protocol.CleanupReport{}, errors.New("cleanup plan digest is required")
	}
	return m.cleanupProject(ctx, ownerID, projectID, scope, false, expectedDigest)
}

func (m *Manager) cleanupProject(
	ctx context.Context,
	ownerID, projectID, scope string,
	dryRun bool,
	expectedDigest string,
) (protocol.CleanupReport, error) {
	report := protocol.CleanupReport{ProjectID: projectID, Scope: scope, DryRun: dryRun}
	if !project.ValidProjectID(projectID) {
		return report, errors.New("project ID is invalid")
	}
	if scope != "results" && scope != "snapshot" && scope != "project" && scope != "cache" {
		return report, errors.New("cleanup scope must be results, snapshot, cache, or project")
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	report.ActiveSessions = m.sessionProjectIDsLocked(ownerID, projectID)
	sort.Strings(report.ActiveSessions)
	if len(report.ActiveSessions) > 0 && !dryRun {
		return report, errors.New("project has live sessions; close them before cleanup")
	}
	records, err := m.projectRecords(ctx, ownerID, projectID)
	if err != nil {
		return report, err
	}
	terminalIDs := make([]string, 0, len(records))
	var resultTargets []cleanupResultTarget
	for _, rec := range records {
		m.mu.Lock()
		_, completing := m.completions[rec.Job.ID]
		m.mu.Unlock()
		if m.active[rec.Job.ID] != nil || completing {
			report.ActiveJobs = append(report.ActiveJobs, rec.Job.ID)
			continue
		}
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
	if scope == "cache" || scope == "project" {
		if len(report.ActiveJobs) > 0 && !dryRun {
			return report, errors.New("project has active jobs; wait for them to finish or cancel queued jobs")
		}
		report.CompileCaches, report.CompileCacheBytes, report.CompileCacheDigest, err = m.projects.CompileCacheStats(
			ownerID,
			projectID,
		)
		if err != nil {
			return report, err
		}
	}
	snapshotID := ""
	if scope == "snapshot" || scope == "project" {
		report.SnapshotPresent, report.SnapshotFiles, report.SnapshotBytes, err = m.projects.SnapshotStats(
			ctx,
			ownerID,
			projectID,
		)
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
	if scope == "cache" || scope == "project" {
		reclaimed, err := m.projects.DeleteCompileCaches(ownerID, projectID)
		if err != nil {
			return report, err
		}
		report.ReclaimedBytes += reclaimed
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

func cleanupReportDigest(
	report protocol.CleanupReport,
	terminalIDs []string,
	resultTargets []cleanupResultTarget,
	snapshotID string,
) (string, error) {
	report.DryRun = false
	report.PlanDigest = ""
	report.ReclaimedBytes = 0
	report.ActiveJobs = append([]string(nil), report.ActiveJobs...)
	sort.Strings(report.ActiveJobs)
	targets := struct {
		Report      protocol.CleanupReport `json:"report"`
		TerminalIDs []string               `json:"terminalJobIds,omitempty"`
		Results     []cleanupResultTarget  `json:"results,omitempty"`
		SnapshotID  string                 `json:"snapshotId,omitempty"`
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
			out = append(
				out,
				record{
					OwnerID: row.OwnerID,
					Job:     protocol.Job{ID: row.ID, ProjectID: row.ProjectID, Status: row.Status},
				},
			)
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
	m.pruneHistoryLocked(ownerID)
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
			if strings.HasPrefix(id, "ses_") {
				id = m.takeSession(id)
			}
			if id != "" {
				m.run(ctx, worker, id)
			}
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
	finishSession := true
	defer func() {
		if finishSession {
			m.finishSession(context.WithoutCancel(ctx), rec)
			m.endPublication(id)
		}
	}()
	if rec.Job.Status != "queued" {
		return
	}
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	m.admissionMu.Lock()
	if err := m.validateSessionJobLocked(rec); err != nil {
		_ = m.cancel(context.WithoutCancel(ctx), id, err.Error())
		m.admissionMu.Unlock()
		return
	}
	now := time.Now().UTC()
	rec.Job.Status, rec.Job.StartedAt = "running", &now
	changed, err := m.transition(ctx, rec, "queued")
	if changed && err == nil {
		m.active[id] = cancelJob
	}
	m.admissionMu.Unlock()
	defer func() { m.admissionMu.Lock(); delete(m.active, id); m.admissionMu.Unlock() }()
	if err != nil {
		m.logger.Error("mark job running", "job_id", id, "error", err)
		finishSession = false
		m.requeue(ctx, id)
		return
	}
	if !changed {
		return
	}
	m.logger.Info("compile job started", "job_id", id, "worker", worker, "owner_id", rec.OwnerID)
	workerStarted := time.Now()
	var materializeTime, executionTime, archiveTime, persistenceTime, cacheTime time.Duration
	defer func() {
		queueTime := time.Duration(0)
		if !rec.Job.CreatedAt.IsZero() {
			queueTime = max(0, now.Sub(rec.Job.CreatedAt))
		}
		m.logger.Info("compile job stages",
			"job_id", id, "session_id", rec.Job.SessionID, "revision", rec.Job.Revision,
			"queue_wait_ms", queueTime.Milliseconds(), "materialize_ms", materializeTime.Milliseconds(),
			"execution_ms", executionTime.Milliseconds(), "result_archive_ms", archiveTime.Milliseconds(),
			"completion_persist_ms", persistenceTime.Milliseconds(), "cache_publish_ms", cacheTime.Milliseconds(),
			"worker_total_ms", time.Since(workerStarted).Milliseconds(),
		)
	}()

	jobWorkspace, err := compile.NewWorkspace(m.cfg.TempDir)
	if err != nil {
		rec, _ = m.finish(ctx, rec, nil, "could not create compile workspace", false)
		return
	}
	defer func() {
		if err := jobWorkspace.Close(); err != nil {
			m.logger.Warn("could not remove compile workspace", "error", err)
		}
	}()
	workspace := jobWorkspace.Project

	if m.cfg.RunnerImage == "" {
		if err := m.projects.Materialize(rec.Snapshot, workspace); err != nil {
			rec, _ = m.finish(ctx, rec, nil, "could not materialize project: "+err.Error(), false)
			return
		}
		materializeTime = time.Since(workerStarted)
	}

	compileCtx, cancelCompile := context.WithTimeout(jobCtx, m.cfg.CompileTimeout)
	defer cancelCompile()
	started := time.Now()
	executed := m.execute(compileCtx, rec, jobWorkspace)
	rec.InvalidateCheckpoint = executed.invalidateCheckpoint
	executionTime = time.Since(started)
	output := executed.output
	output.Result.SessionID, output.Result.Revision = rec.Job.SessionID, rec.Job.Revision
	output.Result.DurationMS = executionTime.Milliseconds()
	output.Result.CompileCache = executed.cache
	output.Result.ServerVersion = m.meta.Version
	output.Result.ImageProfile = m.meta.ImageProfile
	retained := compile.RetainArtifacts(output, rec.Request, m.cfg.ResultRetention)
	archiveStarted := time.Now()
	if _, err := m.projects.WriteResult(rec.OwnerID, rec.Job.ID, retained); err != nil {
		rec, _ = m.finish(ctx, rec, &output.Result, "could not package compile result: "+err.Error(), false)
		return
	}
	archiveTime = time.Since(archiveStarted)
	executed.output = output
	retained.Result.CompileCache = executed.cache
	persistStarted := time.Now()
	completed, success := m.finish(ctx, rec, &retained.Result, retained.Result.Error, true)
	persistenceTime = time.Since(persistStarted)
	rec = completed // Session cleanup uses the confirmed durable terminal state.
	if success {
		cacheStarted := time.Now()
		m.publishExecution(compileCtx, rec, jobWorkspace, &executed)
		rec.InvalidateCheckpoint = executed.invalidateCheckpoint
		m.updatePublishedCache(ctx, rec, executed)
		cacheTime = time.Since(cacheStarted)
	}
}

func (m *Manager) finish(
	ctx context.Context,
	rec record,
	result *protocol.CompileResult,
	message string,
	resultArchived bool,
) (record, bool) {
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
	// Cache publication fills in storage accounting after terminal persistence.
	// Keep the durable/public result immutable while readers can observe it.
	if result != nil {
		copy := *result
		if result.CompileCache != nil {
			cache := *result.CompileCache
			copy.CompileCache = &cache
		}
		rec.Job.Result = &copy
	}
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
		m.deferCompletion(rec)
		return rec, false
	}
	if !changed {
		current, getErr := m.load(persistCtx, rec.Job.ID)
		if getErr != nil || current.Job.FinishedAt == nil {
			m.deferCompletion(rec)
			return rec, false
		}
		m.projects.ReleaseSnapshot(rec.Snapshot.ID)
		m.logger.Warn("compile job state changed before finish", "job_id", rec.Job.ID)
		rec.Job = current.Job
		return rec, current.Job.Status == "succeeded"
	}
	m.projects.ReleaseSnapshot(rec.Snapshot.ID)
	m.logger.Info(
		"compile job finished",
		"job_id",
		rec.Job.ID,
		"status",
		rec.Job.Status,
		"duration_ms",
		resultDuration(result),
	)
	return rec, rec.Job.Status == "succeeded"
}

func (m *Manager) deferCompletion(rec record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.completions == nil {
		m.completions = make(map[string]record)
	}
	if rec.CompletionFrom == "" {
		rec.CompletionFrom = "running"
	}
	m.completions[rec.Job.ID] = rec
}

func (m *Manager) beginPublication(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.publications == nil {
		m.publications = make(map[string]chan struct{})
	}
	if m.publications[id] == nil {
		m.publications[id] = make(chan struct{})
	}
}

func (m *Manager) endPublication(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, pending := m.completions[id]; pending {
		return
	}
	if gate := m.publications[id]; gate != nil {
		close(gate)
		delete(m.publications, id)
		m.publicationVersion++
	}
}

func (m *Manager) cancel(ctx context.Context, id, message string) error {
	rec, err := m.load(ctx, id)
	if err != nil {
		return err
	}
	if rec.Job.Status == "cancelled" {
		return nil
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
	m.endPublication(id)
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
		if current.Job.Status == "queued" && rec.Job.Status != "queued" {
			m.queued--
		}
		if current.Job.Status != "queued" && rec.Job.Status == "queued" {
			m.queued++
		}
		current.Job = rec.Job
		if rec.Job.FinishedAt != nil {
			current.Snapshot = project.Snapshot{}
		}
		m.jobs[rec.Job.ID] = current
		return true, nil
	}
	result, err := marshalResult(rec.Job.Result)
	if err != nil {
		return false, err
	}
	updates := map[string]any{
		"status": rec.Job.Status, "result": result, "error": rec.Job.Error,
		"started_at": rec.Job.StartedAt, "finished_at": rec.Job.FinishedAt,
	}
	if rec.Job.FinishedAt != nil {
		updates["snapshot_manifest"] = nil
	}
	return m.db.TransitionJob(ctx, rec.Job.ID, expectedStatus, updates)
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
	deadline := 5 * time.Second
	if m.cfg.ShutdownTimeout > 0 {
		deadline = min(deadline, m.cfg.ShutdownTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), deadline)
}

// A persistence outage must not occupy a worker or the global admission lock
// indefinitely. Deferred terminal transitions keep their source pin and session
// running slot until durable metadata becomes available again. Restart recovery
// can still safely rerun a job if the process exits before this transition commits.
func (m *Manager) recoverCompletions(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.retryCompletions(ctx)
		}
	}
}

func (m *Manager) retryCompletions(ctx context.Context) {
	m.mu.Lock()
	pending := make([]record, 0, len(m.completions))
	for _, rec := range m.completions {
		pending = append(pending, rec)
	}
	m.mu.Unlock()
	for _, rec := range pending {
		if ctx.Err() != nil {
			return
		}
		operation, cancel := m.persistenceContext(ctx)
		changed, err := m.transition(operation, rec, rec.CompletionFrom)
		if err == nil && !changed {
			var current record
			current, err = m.load(operation, rec.Job.ID)
			if err == nil {
				if current.Job.FinishedAt == nil {
					err = errors.New("completion is not terminal")
				} else {
					rec.Job = current.Job
				}
			}
		}
		cancel()
		if err != nil {
			continue
		}
		m.mu.Lock()
		delete(m.completions, rec.Job.ID)
		m.mu.Unlock()
		if rec.CompletionFrom == "running" {
			m.projects.ReleaseSnapshot(rec.Snapshot.ID)
		}
		m.finishSession(ctx, rec)
		m.endPublication(rec.Job.ID)
		if ctx.Err() != nil {
			return
		}
	}
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
	m.mu.Lock()
	protected := make([]string, 0, len(m.publications))
	for id := range m.publications {
		protected = append(protected, id)
	}
	m.mu.Unlock()
	if m.db != nil {
		operation, cancel := m.persistenceContext(ctx)
		defer cancel()
		removed, err := m.db.DeleteTerminalJobsBefore(operation, cutoff, protected)
		if err != nil {
			m.logger.Error("terminal job metadata sweep failed", "error", err)
		} else if removed > 0 {
			m.logger.Info("terminal job metadata swept", "jobs", removed)
		}
		return
	}
	m.mu.Lock()
	removed := 0
	owners := make(map[string]struct{})
	for id, rec := range m.jobs {
		terminal := rec.Job.Status == "succeeded" || rec.Job.Status == "failed" || rec.Job.Status == "cancelled"
		if terminal && m.publications[id] == nil && rec.Job.FinishedAt != nil && rec.Job.FinishedAt.Before(cutoff) {
			delete(m.jobs, id)
			owners[rec.OwnerID] = struct{}{}
			removed++
		}
	}
	for ownerID := range owners {
		m.pruneHistoryLocked(ownerID)
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
		m.indexHistoryLocked(rec)
		if rec.Job.Status == "queued" {
			m.queued++
		}
		if m.publications == nil {
			m.publications = make(map[string]chan struct{})
		}
		m.publications[rec.Job.ID] = make(chan struct{})
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
	err = m.db.CreateJob(
		ctx,
		store.CompileJob{
			SessionID: rec.Job.SessionID, Revision: rec.Job.Revision,
			ID:               rec.Job.ID,
			OwnerID:          rec.OwnerID,
			ProjectID:        rec.Job.ProjectID,
			SnapshotID:       rec.Snapshot.ID,
			SnapshotManifest: snapshot,
			Status:           rec.Job.Status,
			Request:          request,
			Result:           result,
			Error:            rec.Job.Error,
			CreatedAt:        rec.Job.CreatedAt,
			StartedAt:        rec.Job.StartedAt,
			FinishedAt:       rec.Job.FinishedAt,
		},
	)
	if err == nil {
		m.beginPublication(rec.Job.ID)
	}
	return err
}

// Native history is ordered by immutable creation time, then identifier. Reads
// copy only the requested page; completion transitions never rebuild the index.
func (m *Manager) indexHistoryLocked(rec record) {
	if m.jobHistory == nil {
		m.jobHistory = make(map[string][]string)
	}
	history := m.jobHistory[rec.OwnerID]
	position := sort.Search(len(history), func(i int) bool {
		job := m.jobs[history[i]].Job
		return job.CreatedAt.After(rec.Job.CreatedAt) ||
			(job.CreatedAt.Equal(rec.Job.CreatedAt) && job.ID > rec.Job.ID)
	})
	m.jobHistory[rec.OwnerID] = slices.Insert(history, position, rec.Job.ID)
}

func (m *Manager) pruneHistoryLocked(ownerID string) {
	history := slices.DeleteFunc(m.jobHistory[ownerID], func(id string) bool {
		_, present := m.jobs[id]
		return !present
	})
	if len(history) == 0 {
		delete(m.jobHistory, ownerID)
	} else {
		m.jobHistory[ownerID] = history
	}
}

func marshalResult(result *protocol.CompileResult) ([]byte, error) {
	if result == nil {
		return nil, nil
	}
	return json.Marshal(result)
}

func recordFromRow(row store.CompileJob) (record, error) {
	var request protocol.CompileRequest
	if err := json.Unmarshal(row.Request, &request); err != nil {
		return record{}, fmt.Errorf("decode queued job request: %w", err)
	}
	job := protocol.Job{
		SessionID: row.SessionID, Revision: row.Revision,
		ID:         row.ID,
		SnapshotID: row.SnapshotID,
		ProjectID:  row.ProjectID,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt,
		StartedAt:  row.StartedAt,
		FinishedAt: row.FinishedAt,
		Error:      row.Error,
	}
	if len(row.Result) > 0 {
		var result protocol.CompileResult
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

func resultDuration(result *protocol.CompileResult) int64 {
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

func withoutExpiredAuxiliary(job protocol.Job) protocol.Job {
	if job.Result == nil || job.Result.AuxiliaryExpiresAt == nil || time.Now().Before(*job.Result.AuxiliaryExpiresAt) {
		return job
	}
	result := *job.Result
	result.Artifacts = nil
	for _, artifact := range job.Result.Artifacts {
		if artifact.Kind != "auxiliary" {
			result.Artifacts = append(result.Artifacts, artifact)
		}
	}
	job.Result = &result
	return job
}
