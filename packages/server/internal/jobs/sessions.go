package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/project"
)

var (
	ErrSessionNotFound  = errors.New("realtime session not found or expired")
	ErrRevisionConflict = errors.New("session revision conflict; refresh session state")
)

type revisionReceipt struct {
	request api.RevisionRequest
	jobID   string
}
type liveSession struct {
	ownerID      string
	request      api.CompileRequest
	state        api.Session
	snapshot     project.Snapshot
	scheduled    bool
	receipts     map[string]revisionReceipt
	receiptOrder []string
	events       []api.SessionEvent
	changed      chan struct{}
}

func (m *Manager) CreateSession(ctx context.Context, ownerID string, req api.SessionRequest) (api.Session, error) {
	if m.cfg.MaxRealtimeSessions <= 0 {
		return api.Session{}, errors.New("realtime sessions are disabled")
	}
	if !project.ValidProjectID(req.ProjectID) {
		return api.Session{}, errors.New("invalid project ID")
	}
	if req.Workspace != "fresh" && req.Workspace != "reuse" {
		return api.Session{}, errors.New("workspace must be fresh or reuse")
	}
	if req.Workspace == "reuse" && (m.cfg.RunnerImage == "" || req.Request.Auxiliary.Server != "reuse") {
		return api.Session{}, errors.New("workspace reuse requires an isolated runner and auxiliary.server=reuse")
	}
	if err := m.runner.ValidateRequest(req.Request); err != nil {
		return api.Session{}, err
	}
	if req.Workspace == "reuse" && req.Request.ShellEscape {
		return api.Session{}, errors.New("realtime workspace reuse does not allow shell escape")
	}
	if err := ctx.Err(); err != nil {
		return api.Session{}, err
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	m.expireSessionsLocked(ctx)
	if len(m.sessions) >= m.cfg.MaxRealtimeSessions {
		return api.Session{}, errors.New("realtime session capacity exhausted")
	}
	id, err := randomID("ses")
	if err != nil {
		return api.Session{}, err
	}
	s := &liveSession{ownerID: ownerID, request: req.Request, state: api.Session{ID: id, ProjectID: req.ProjectID, Workspace: req.Workspace, ExpiresAt: time.Now().UTC().Add(m.cfg.RealtimeSessionTTL)}, receipts: make(map[string]revisionReceipt), changed: make(chan struct{})}
	m.sessions[id] = s
	m.publishSessionLocked(s, "created", api.Job{})
	return s.state, nil
}

func (m *Manager) sessionLocked(ownerID, id string) (*liveSession, error) {
	s := m.sessions[id]
	if s == nil || s.ownerID != ownerID || time.Now().After(s.state.ExpiresAt) {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

func (m *Manager) GetSession(ctx context.Context, ownerID, id string) (api.Session, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	m.expireSessionsLocked(ctx)
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return api.Session{}, err
	}
	s.state.ExpiresAt = time.Now().UTC().Add(m.cfg.RealtimeSessionTTL)
	return s.state, nil
}

// SubmitRevision atomically chooses the next wanted snapshot. Only one queue
// token exists per idle session, so rapid replacements cannot fill the channel.
func (m *Manager) SubmitRevision(ctx context.Context, ownerID, id string, req api.RevisionRequest) (api.Job, error) {
	if len(req.IdempotencyKey) < 16 || len(req.IdempotencyKey) > 64 || strings.ContainsAny(req.IdempotencyKey, "\r\n\x00") {
		return api.Job{}, errors.New("idempotencyKey must have 16-64 characters without control characters")
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return api.Job{}, err
	}
	if receipt, ok := s.receipts[req.IdempotencyKey]; ok {
		if receipt.request != req {
			return api.Job{}, errors.New("idempotency key was used for a different revision")
		}
		return m.Get(ctx, ownerID, receipt.jobID)
	}
	if req.BaseRevision != s.state.Revision || s.state.Revision >= 1<<31 {
		return api.Job{}, ErrRevisionConflict
	}
	snapshot, request, err := m.projects.PrepareUpload(ownerID, req.UploadID)
	if err != nil {
		return api.Job{}, err
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
		return api.Job{}, errors.New("upload project or compile options do not match the session")
	}
	for _, file := range snapshot.Files {
		if file.Path == ".latexmk-build" || strings.HasPrefix(file.Path, ".latexmk-build/") || file.Path == ".latexmk-home" || strings.HasPrefix(file.Path, ".latexmk-home/") {
			return api.Job{}, errors.New("source uses a reserved runner directory")
		}
	}
	pending, err := m.pendingCount(ctx)
	if err != nil {
		return api.Job{}, err
	}
	if s.state.PendingJobID != "" {
		pending--
	}
	if pending >= m.cfg.MaxQueuedJobs {
		return api.Job{}, errors.New("compile queue is full")
	}
	jobID, err := randomID("job")
	if err != nil {
		return api.Job{}, err
	}
	rec := record{OwnerID: ownerID, Snapshot: snapshot, Request: request, Job: api.Job{ID: jobID, ProjectID: snapshot.ProjectID, SnapshotID: snapshot.ID, SessionID: id, Revision: s.state.Revision + 1, Status: "queued", CreatedAt: time.Now().UTC()}}
	if err := m.save(ctx, rec); err != nil {
		return api.Job{}, err
	}
	if s.state.PendingJobID != "" {
		if err := m.cancel(ctx, s.state.PendingJobID, "superseded by a newer session revision"); err != nil {
			_ = m.cancel(context.WithoutCancel(ctx), jobID, "session admission failed")
			return api.Job{}, err
		}
	}
	// One pin belongs to the job and one to the live session's current source tree.
	if err := m.projects.PinSnapshot(snapshot); err != nil {
		return api.Job{}, err
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
	m.publishSessionLocked(s, "running", api.Job{ID: jobID, Revision: s.state.Revision, Status: "running"})
	return jobID
}

func (m *Manager) finishSession(ctx context.Context, rec record) {
	if rec.Job.SessionID == "" {
		return
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s := m.sessions[rec.Job.SessionID]
	if s == nil || s.state.RunningJobID != rec.Job.ID {
		return
	}
	job, err := m.Get(ctx, rec.OwnerID, rec.Job.ID)
	if err != nil {
		m.logger.Error("read session completion", "error", err)
		job = rec.Job
	}
	s.state.RunningJobID = ""
	if job.Status == "succeeded" {
		s.state.LastSuccessfulJobID = job.ID
	}
	m.publishSessionLocked(s, "finished", job)
	m.scheduleSessionLocked(s)
}

func (m *Manager) publishSessionLocked(s *liveSession, kind string, job api.Job) {
	s.state.EventSequence++
	s.events = append(s.events, api.SessionEvent{Sequence: s.state.EventSequence, Type: kind, Revision: job.Revision, JobID: job.ID, Status: job.Status})
	if len(s.events) > 64 {
		s.events = s.events[len(s.events)-64:]
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// Events returns a bounded replay plus a broadcast notification. Subscribers
// never run on a compile worker, and slow clients cannot backpressure compiles.
func (m *Manager) SessionEvents(ownerID, id string, after uint64) ([]api.SessionEvent, <-chan struct{}, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return nil, nil, err
	}
	var events []api.SessionEvent
	if after > s.state.EventSequence || (len(s.events) > 0 && after+1 < s.events[0].Sequence) {
		events = []api.SessionEvent{{Sequence: s.state.EventSequence, Type: "resync", Revision: s.state.Revision}}
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
	s, err := m.sessionLocked(ownerID, id)
	if err != nil {
		return err
	}
	return m.closeSessionLocked(ctx, s)
}

func (m *Manager) closeSessionLocked(ctx context.Context, s *liveSession) error {
	if s.state.PendingJobID != "" {
		if err := m.cancel(ctx, s.state.PendingJobID, "session closed"); err != nil {
			return err
		}
	}
	if cancel := m.active[s.state.RunningJobID]; cancel != nil {
		cancel()
	}
	if s.snapshot.ID != "" {
		m.projects.ReleaseSnapshot(s.snapshot.ID)
	}
	close(s.changed)
	delete(m.sessions, s.state.ID)
	return nil
}

func (m *Manager) expireSessionsLocked(ctx context.Context) {
	for _, s := range m.sessions {
		if time.Now().After(s.state.ExpiresAt) {
			if err := m.closeSessionLocked(ctx, s); err != nil {
				m.logger.Warn("expire session", "error", err)
			}
		}
	}
}

func (m *Manager) maintainSessions(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		m.admissionMu.Lock()
		defer m.admissionMu.Unlock()
		for _, s := range m.sessions {
			_ = m.closeSessionLocked(context.WithoutCancel(ctx), s)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.admissionMu.Lock()
			m.expireSessionsLocked(ctx)
			for _, s := range m.sessions {
				m.scheduleSessionLocked(s)
			}
			m.admissionMu.Unlock()
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

func (m *Manager) validateSessionJob(rec record) error {
	if rec.Job.SessionID == "" {
		return nil
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	s, err := m.sessionLocked(rec.OwnerID, rec.Job.SessionID)
	if err != nil {
		return err
	}
	if s.state.RunningJobID != rec.Job.ID {
		return fmt.Errorf("session no longer owns this compile attempt")
	}
	return nil
}
