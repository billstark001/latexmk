package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/sandbox"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

var (
	ErrRevisionRate     = errors.New("realtime revision rate exhausted")
	ErrSessionNotFound  = errors.New("realtime session not found or expired")
	ErrSessionCapacity  = errors.New("realtime session capacity exhausted")
	ErrQueueCapacity    = errors.New("compile queue is full")
	ErrRevisionConflict = errors.New("session revision conflict; refresh session state")
)

type revisionReceipt struct {
	request protocol.RevisionRequest
	jobID   string
}
type liveSession struct {
	creationKey  string
	subscribers  int
	cacheExpires time.Time
	cachePath    string
	cacheInputs  []protocol.ProjectFile
	cacheHashes  map[string]string
	cacheStamps  map[string]int64
	ownerID      string
	request      protocol.CompileRequest
	state        protocol.Session
	snapshot     project.Snapshot
	scheduled    bool
	receipts     map[string]revisionReceipt
	receiptOrder []string
	events       []protocol.SessionEvent
	changed      chan struct{}
}

func (m *Manager) CreateSession(
	ctx context.Context,
	ownerID string,
	req protocol.SessionRequest,
) (protocol.Session, error) {
	if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
		return protocol.Session{}, err
	}
	if m.cfg.MaxRealtimeSessions <= 0 {
		return protocol.Session{}, errors.New("realtime sessions are disabled")
	}
	if !project.ValidProjectID(req.ProjectID) {
		return protocol.Session{}, errors.New("invalid project ID")
	}
	if req.Workspace != "fresh" && req.Workspace != "reuse" {
		return protocol.Session{}, errors.New("workspace must be fresh or reuse")
	}
	if req.Workspace == "reuse" &&
		(m.cfg.RunnerImage == "" || req.Request.Auxiliary.Server != "reuse" || m.cfg.MaxCompileCacheBytes <= 0) {
		return protocol.Session{}, errors.New("workspace reuse requires an isolated runner and auxiliary.server=reuse")
	}
	if err := m.runner.ValidateRequest(req.Request); err != nil {
		return protocol.Session{}, err
	}
	if m.cfg.RunnerImage != "" && req.Request.ShellEscape {
		return protocol.Session{}, errors.New("isolated realtime workspaces do not allow shell escape")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Session{}, err
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	m.expireSessionsLocked(ctx)
	for _, s := range m.sessions {
		if s.ownerID != ownerID || s.creationKey != req.IdempotencyKey {
			continue
		}
		expected, _ := json.Marshal(s.request)
		actual, _ := json.Marshal(req.Request)
		if s.state.ProjectID != req.ProjectID || s.state.Workspace != req.Workspace ||
			string(expected) != string(actual) {
			return protocol.Session{}, errors.New("idempotency key was used for a different session")
		}
		return s.state, nil
	}
	if len(m.sessions) >= m.cfg.MaxRealtimeSessions {
		return protocol.Session{}, ErrSessionCapacity
	}
	ownerSessions := 0
	for _, s := range m.sessions {
		if s.ownerID == ownerID {
			ownerSessions++
		}
	}
	if m.cfg.MaxRealtimeSessionsPerOwner > 0 && ownerSessions >= m.cfg.MaxRealtimeSessionsPerOwner {
		return protocol.Session{}, ErrSessionCapacity
	}
	id, err := randomID("ses")
	if err != nil {
		return protocol.Session{}, err
	}
	s := &liveSession{
		creationKey: req.IdempotencyKey,
		ownerID:     ownerID,
		request:     req.Request,
		state: protocol.Session{
			ID:        id,
			ProjectID: req.ProjectID,
			Workspace: req.Workspace,
			ExpiresAt: time.Now().UTC().Add(m.cfg.RealtimeSessionTTL),
		},
		receipts: make(map[string]revisionReceipt),
		changed:  make(chan struct{}),
	}
	m.sessions[id] = s
	m.publishSessionLocked(s, "created", protocol.Job{})
	return s.state, nil
}

func (m *Manager) sessionLocked(ownerID, id string) (*liveSession, error) {
	s := m.sessions[id]
	if s == nil || s.ownerID != ownerID || time.Now().After(s.state.ExpiresAt) {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

func (m *Manager) GetSession(ctx context.Context, ownerID, id string) (protocol.Session, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	m.expireSessionsLocked(ctx)
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return protocol.Session{}, err
	}
	return s.state, nil
}

// RenewSession extends the lease only in response to an explicit client request.
// Reading state or keeping an SSE transport open is not evidence of client activity.
func (m *Manager) RenewSession(ctx context.Context, ownerID, id string) (protocol.Session, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	m.expireSessionsLocked(ctx)
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return protocol.Session{}, err
	}
	s.state.ExpiresAt = time.Now().UTC().Add(m.cfg.RealtimeSessionTTL)
	return s.state, nil
}

// SubmitRevision atomically chooses the next wanted snapshot. Only one queue
// token exists per idle session, so rapid replacements cannot fill the channel.
func (m *Manager) SubmitRevision(
	ctx context.Context,
	ownerID, id string,
	req protocol.RevisionRequest,
) (protocol.Job, error) {
	if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
		return protocol.Job{}, err
	}
	m.admissionMu.Lock()
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		m.admissionMu.Unlock()
		return protocol.Job{}, err
	}
	if receipt, ok := s.receipts[req.IdempotencyKey]; ok {
		m.admissionMu.Unlock()
		if receipt.request != req {
			return protocol.Job{}, errors.New("idempotency key was used for a different revision")
		}
		return m.Get(ctx, ownerID, receipt.jobID)
	}
	defer m.admissionMu.Unlock()
	if req.BaseRevision != s.state.Revision || s.state.Revision >= 1<<31 {
		return protocol.Job{}, ErrRevisionConflict
	}
	if !m.allowRevisionLocked(ownerID) {
		return protocol.Job{}, ErrRevisionRate
	}
	snapshot, request, err := m.projects.PrepareUpload(ownerID, req.UploadID)
	if err != nil {
		return protocol.Job{}, err
	}
	transferred := false
	defer func() {
		if !transferred {
			m.projects.ReleaseSnapshot(snapshot.ID)
		}
	}()
	expected, _ := json.Marshal(s.request)
	actual, _ := json.Marshal(request)
	if snapshot.ProjectID != s.state.ProjectID || string(expected) != string(actual) {
		return protocol.Job{}, errors.New("upload project or compile options do not match the session")
	}
	if err := sandbox.ValidateSourcePaths(snapshot.Files); err != nil {
		return protocol.Job{}, err
	}
	pending, err := m.pendingCount(ctx)
	if err != nil {
		return protocol.Job{}, err
	}
	if s.state.PendingJobID != "" {
		previous, err := m.Get(ctx, ownerID, s.state.PendingJobID)
		if err != nil {
			return protocol.Job{}, err
		}
		if previous.Status == "queued" {
			pending--
		}
	}
	if pending >= m.cfg.MaxQueuedJobs {
		return protocol.Job{}, ErrQueueCapacity
	}
	jobID, err := randomID("job")
	if err != nil {
		return protocol.Job{}, err
	}
	rec := record{
		OwnerID:  ownerID,
		Snapshot: snapshot,
		Request:  request,
		Job: protocol.Job{
			ID:         jobID,
			ProjectID:  snapshot.ProjectID,
			SnapshotID: snapshot.ID,
			SessionID:  id,
			Revision:   s.state.Revision + 1,
			Status:     "queued",
			CreatedAt:  time.Now().UTC(),
		},
	}
	if err := m.save(ctx, rec); err != nil {
		return protocol.Job{}, err
	}
	transferred = true // The persisted job now owns the prepared snapshot pin.
	if s.state.PendingJobID != "" {
		if err := m.cancel(ctx, s.state.PendingJobID, "superseded by a newer session revision"); err != nil {
			_ = m.cancel(context.WithoutCancel(ctx), jobID, "session admission failed")
			return protocol.Job{}, err
		}
	}
	// One pin belongs to the job and one to the live session's current source tree.
	if err := m.projects.PinSnapshot(snapshot); err != nil {
		_ = m.cancel(context.WithoutCancel(ctx), jobID, "session snapshot pin failed")
		return protocol.Job{}, err
	}
	if s.snapshot.ID != "" {
		m.projects.ReleaseSnapshot(s.snapshot.ID)
	}
	s.snapshot = snapshot
	s.state.Revision = rec.Job.Revision
	s.state.LatestJobID = jobID
	s.state.PendingJobID = jobID
	s.state.ExpiresAt = time.Now().UTC().Add(m.cfg.RealtimeSessionTTL)
	s.receipts[req.IdempotencyKey] = revisionReceipt{request: req, jobID: jobID}
	s.receiptOrder = append(s.receiptOrder, req.IdempotencyKey)
	if len(s.receiptOrder) > 128 {
		delete(s.receipts, s.receiptOrder[0])
		s.receiptOrder = s.receiptOrder[1:]
	}
	transferred = true
	m.projects.ConsumeUpload(ownerID, req.UploadID)
	m.publishSessionLocked(s, "submitted", rec.Job)
	m.scheduleSessionLocked(s)
	return rec.Job, nil
}

func validateIdempotencyKey(key string) error {
	if len(key) < 16 || len(key) > 64 || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return errors.New("idempotencyKey must have 16-64 characters without control characters")
	}
	return nil
}

func (m *Manager) scheduleSessionLocked(s *liveSession) {
	if s.scheduled || s.state.RunningJobID != "" || s.state.PendingJobID == "" {
		return
	}
	select {
	case m.queue <- s.state.ID:
		s.scheduled = true
	default:
	}
	// A full physical channel can contain cancelled legacy jobs. Workers and the
	// maintenance loop retry scheduling rather than blocking admission.
}

func (m *Manager) takeSession(id string) string {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return ""
	}
	s.scheduled = false
	if s.state.RunningJobID != "" || s.state.PendingJobID == "" {
		return ""
	}
	jobID := s.state.PendingJobID
	s.state.PendingJobID = ""
	s.state.RunningJobID = jobID
	m.publishSessionLocked(s, "running", protocol.Job{ID: jobID, Revision: s.state.Revision, Status: "running"})
	return jobID
}

func (m *Manager) finishSession(ctx context.Context, rec record) {
	if rec.Job.SessionID == "" {
		return
	}
	m.mu.Lock()
	_, pending := m.completions[rec.Job.ID]
	m.mu.Unlock()
	if pending {
		return
	}
	job := rec.Job
	if job.FinishedAt == nil {
		operation, cancel := m.persistenceContext(ctx)
		var err error
		var completed record
		completed, err = m.load(operation, rec.Job.ID)
		job = completed.Job
		cancel()
		if err != nil {
			m.logger.Error("read session completion", "error", err)
			return
		}
	}
	if job.FinishedAt == nil {
		return
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s := m.sessions[rec.Job.SessionID]
	if s == nil || s.state.RunningJobID != rec.Job.ID {
		return
	}
	s.state.RunningJobID = ""
	if job.Status == "succeeded" {
		s.state.LastSuccessfulJobID = job.ID
	}
	if rec.InvalidateCheckpoint {
		m.clearSessionCacheLocked(s)
	}
	m.publishSessionLocked(s, "finished", job)
	m.scheduleSessionLocked(s)
}

func (m *Manager) publishSessionLocked(s *liveSession, kind string, job protocol.Job) {
	s.state.EventSequence++
	s.events = append(
		s.events,
		protocol.SessionEvent{
			Sequence: s.state.EventSequence,
			Type:     kind,
			Revision: job.Revision,
			JobID:    job.ID,
			Status:   job.Status,
		},
	)
	if len(s.events) > 64 {
		s.events = s.events[len(s.events)-64:]
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// SessionEvents returns a bounded replay plus a broadcast notification. Subscribers
// never run on a compile worker, and slow clients cannot backpressure compiles.
func (m *Manager) SessionEvents(ownerID, id string, after uint64) ([]protocol.SessionEvent, <-chan struct{}, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return nil, nil, err
	}
	var events []protocol.SessionEvent
	if after > s.state.EventSequence || (len(s.events) > 0 && after+1 < s.events[0].Sequence) {
		events = []protocol.SessionEvent{{Sequence: s.state.EventSequence, Type: "resync", Revision: s.state.Revision}}
	} else {
		for _, event := range s.events {
			if event.Sequence > after {
				events = append(events, event)
			}
		}
	}
	return events, s.changed, nil
}

func (m *Manager) CloseSession(ctx context.Context, ownerID, id string) error {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	operation, cancel := m.persistenceContext(ctx)
	defer cancel()
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return err
	}
	return m.closeSessionLocked(operation, s)
}

func (m *Manager) closeSessionLocked(ctx context.Context, s *liveSession) error {
	if s.state.PendingJobID != "" {
		if err := m.cancel(ctx, s.state.PendingJobID, "session closed"); err != nil {
			return err
		}
	}
	cancel := m.active[s.state.RunningJobID]
	m.mu.Lock()
	_, completing := m.completions[s.state.RunningJobID]
	m.mu.Unlock()
	if cancel != nil || completing {
		rec, err := m.load(ctx, s.state.RunningJobID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		rec.Job.Status, rec.Job.Error, rec.Job.FinishedAt = "cancelled", "session closed", &now
		if _, err := m.transition(ctx, rec, "running"); err != nil {
			return err
		}
		if cancel != nil {
			cancel()
		}
	}
	if s.snapshot.ID != "" {
		m.projects.ReleaseSnapshot(s.snapshot.ID)
	}
	m.clearSessionCacheLocked(s)
	close(s.changed)
	delete(m.sessions, s.state.ID)
	return nil
}

func (m *Manager) expireSessionsLocked(ctx context.Context) {
	// Lease cleanup may touch durable running jobs. One shared budget bounds an
	// entire sweep so an expired session cannot indefinitely hold admission
	// during a database outage, including sweeps triggered by other owners.
	operation, cancel := m.persistenceContext(ctx)
	defer cancel()
	for owner, budget := range m.revisionBudgets {
		if time.Since(budget.updated) > m.cfg.RealtimeSessionTTL {
			delete(m.revisionBudgets, owner)
		}
	}
	for _, s := range m.sessions {
		if s.cachePath != "" && time.Now().After(s.cacheExpires) {
			m.clearSessionCacheLocked(s)
		}
		if time.Now().After(s.state.ExpiresAt) {
			if err := m.closeSessionLocked(operation, s); err != nil {
				m.logger.Warn("expire session", "error", err)
			}
		}
	}
}

func (m *Manager) maintainSessions(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		operation, cancel := m.persistenceContext(ctx)
		defer cancel()
		m.admissionMu.Lock()
		defer m.admissionMu.Unlock()
		for _, s := range m.sessions {
			_ = m.closeSessionLocked(operation, s)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			operation, cancel := m.persistenceContext(ctx)
			m.admissionMu.Lock()
			m.expireSessionsLocked(operation)
			for _, s := range m.sessions {
				m.scheduleSessionLocked(s)
			}
			m.admissionMu.Unlock()
			cancel()
		}
	}
}

func (m *Manager) sessionProjectIDsLocked(ownerID, projectID string) []string {
	var ids []string
	for _, s := range m.sessions {
		if s.ownerID == ownerID && s.state.ProjectID == projectID {
			ids = append(ids, s.state.ID)
		}
	}
	return ids
}

func (m *Manager) validateSessionJobLocked(rec record) error {
	if rec.Job.SessionID == "" {
		return nil
	}
	s, err := m.sessionLocked(rec.OwnerID, rec.Job.SessionID)
	if err != nil {
		return err
	}
	if s.state.RunningJobID != rec.Job.ID {
		return fmt.Errorf("session no longer owns this compile attempt")
	}
	return nil
}

// SubscribeSession bounds streaming connections independently from job workers.
func (m *Manager) SubscribeSession(ownerID, id string) (func(), error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return nil, err
	}
	if s.subscribers >= 4 {
		return nil, ErrSessionCapacity
	}
	s.subscribers++
	return func() { m.admissionMu.Lock(); defer m.admissionMu.Unlock(); s.subscribers-- }, nil
}

type revisionBudget struct {
	tokens  float64
	updated time.Time
}

func (m *Manager) allowRevisionLocked(owner string) bool {
	rate := m.cfg.MaxRealtimeRevisionRate
	if rate <= 0 {
		return true
	}
	now := time.Now()
	burst := float64(rate * 2)
	budget := m.revisionBudgets[owner]
	if budget == nil {
		budget = &revisionBudget{tokens: burst, updated: now}
		m.revisionBudgets[owner] = budget
	}
	budget.tokens = min(burst, budget.tokens+now.Sub(budget.updated).Seconds()*float64(rate))
	budget.updated = now
	if budget.tokens < 1 {
		return false
	}
	budget.tokens--
	return true
}
