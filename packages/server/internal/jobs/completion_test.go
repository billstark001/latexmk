package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/store"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestSuccessfulReadsAndReceiptReplayWaitForCachePublication(t *testing.T) {
	m, req := sessionManager(t)
	ctx := context.Background()
	session, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	payload := planRevision(t, m, req, "source", 0)
	job, err := m.SubmitRevision(ctx, "owner", session.ID, payload)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := m.load(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	rec.Job.Status, rec.Job.FinishedAt = "succeeded", &finished
	rec.Job.Result = &protocol.CompileResult{Success: true, CompileCache: &protocol.CompileCache{StoredFiles: 0}}
	if _, err := m.transition(ctx, rec, "queued"); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := m.Get(deadline, "owner", job.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("incomplete cache accounting escaped publication gate: %v", err)
	}
	replayed := make(chan protocol.Job, 1)
	replayErrors := make(chan error, 1)
	go func() {
		operation, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		job, err := m.SubmitRevision(operation, "owner", session.ID, payload)
		replayed <- job
		replayErrors <- err
	}()
	read := make(chan error, 1)
	go func() { _, err := m.GetSession(ctx, "owner", session.ID); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("receipt replay waited while holding global admission")
	}
	rec.Job.Result = &protocol.CompileResult{Success: true, CompileCache: &protocol.CompileCache{StoredFiles: 3}}
	if _, err := m.transition(ctx, rec, "succeeded"); err != nil {
		t.Fatal(err)
	}
	m.endPublication(job.ID)
	got := <-replayed
	if err := <-replayErrors; err != nil || got.Result == nil || got.Result.CompileCache.StoredFiles != 3 {
		t.Fatalf("replay observed incomplete cache publication: %+v, %v", got, err)
	}
	listed, err := m.List(ctx, "owner", 10)
	if err != nil || len(listed) != 1 || listed[0].Result.CompileCache.StoredFiles != 3 {
		t.Fatalf("list observed incomplete cache publication: %+v, %v", listed, err)
	}
}

func TestRetentionCannotDeleteAnUnpublishedTerminalJob(t *testing.T) {
	m, _ := sessionManager(t)
	finished := time.Now().UTC().Add(-time.Hour)
	rec := record{OwnerID: "owner", Job: protocol.Job{ID: "job_publishing", Status: "succeeded", FinishedAt: &finished}}
	if err := m.save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	m.pruneTerminal(context.Background(), time.Now())
	if _, err := m.load(context.Background(), rec.Job.ID); err != nil {
		t.Fatal("retention deleted a result before cache accounting published")
	}
	m.endPublication(rec.Job.ID)
	m.pruneTerminal(context.Background(), time.Now())
	if _, err := m.load(context.Background(), rec.Job.ID); err == nil {
		t.Fatal("completed publication was protected forever")
	}
}

type pruneFaultStore struct {
	*store.Postgres
	started chan struct{}
}

type historyPublicationStore struct {
	*store.Postgres
	manager *Manager
	row     store.CompileJob
	churn   bool
	lists   int
	reads   int
}

func (s *historyPublicationStore) ListJobs(context.Context, string, int) ([]store.CompileJob, error) {
	s.lists++
	if s.lists == 1 || s.churn {
		stale := s.row
		stale.Result = []byte(`{"success":true,"compileCache":{"storedFiles":0}}`)
		// Reproduce a job that starts and finishes publishing during the SQL
		// query, after the list caller's initial publication snapshot.
		s.manager.beginPublication(s.row.ID)
		s.manager.endPublication(s.row.ID)
		return []store.CompileJob{stale}, nil
	}
	return []store.CompileJob{s.row}, nil
}

func (s *historyPublicationStore) GetJob(context.Context, string) (store.CompileJob, error) {
	s.reads++
	return s.row, nil
}

func TestDatabaseListReconcilesPublicationsCompletedDuringQuery(t *testing.T) {
	for _, churn := range []bool{false, true} {
		t.Run(fmt.Sprintf("churn=%t", churn), func(t *testing.T) {
			m, req := sessionManager(t)
			request, err := json.Marshal(req.Request)
			if err != nil {
				t.Fatal(err)
			}
			finished := time.Now().UTC()
			fake := &historyPublicationStore{
				manager: m, churn: churn,
				row: store.CompileJob{ID: "job_list", OwnerID: "owner", Status: "succeeded",
					Request: request, FinishedAt: &finished,
					Result: []byte(`{"success":true,"compileCache":{"storedFiles":3}}`)},
			}
			m.db = fake
			listed, err := m.List(context.Background(), "owner", 10)
			if err != nil || len(listed) != 1 || listed[0].Result.CompileCache.StoredFiles != 3 {
				t.Fatalf("list published stale cache accounting: %+v, %v", listed, err)
			}
			wantLists, wantReads := 2, 0
			if churn {
				wantLists, wantReads = 3, 1
			}
			if fake.lists != wantLists || fake.reads != wantReads {
				t.Fatalf("unbounded/redundant reads: lists=%d reads=%d", fake.lists, fake.reads)
			}
		})
	}
}

func (s *pruneFaultStore) DeleteTerminalJobsBefore(ctx context.Context, _ time.Time, _ []string) (int64, error) {
	close(s.started)
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestDatabaseRetentionOutageDoesNotBlockSessions(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.ShutdownTimeout = 150 * time.Millisecond
	session, err := m.CreateSession(context.Background(), "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	fault := &pruneFaultStore{started: make(chan struct{})}
	m.db = fault
	done := make(chan struct{})
	go func() { m.pruneTerminal(context.Background(), time.Now()); close(done) }()
	select {
	case <-fault.started:
	case <-time.After(time.Second):
		t.Fatal("retention query did not start")
	}
	read := make(chan error, 1)
	go func() { _, err := m.GetSession(context.Background(), "owner", session.ID); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("retention query held global admission")
	}
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("retention query had no bounded deadline")
	}
}

func TestClosedSessionDiscardsLateCheckpointPublication(t *testing.T) {
	m, req := sessionManager(t)
	ctx := context.Background()
	session, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseSession(ctx, "owner", session.ID); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.tar.gz")
	if err := os.WriteFile(checkpoint, []byte("verified checkpoint transport"), 0600); err != nil {
		t.Fatal(err)
	}
	executed := execution{sessionCheckpoint: true, checkpoint: checkpoint, cache: &protocol.CompileCache{}}
	executed.output.Result.Success = true
	rec := record{OwnerID: "owner", Job: protocol.Job{ID: "job_late", SessionID: session.ID}}
	m.publishExecution(ctx, rec, nil, &executed)
	path, err := m.projects.LiveCachePath("owner", session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed session acquired a new checkpoint: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatalf("discarded checkpoint staging leaked: %v %v", entries, err)
	}
}

func TestPublishedExpiryDoesNotAliasMutableSessionCache(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.CompileCacheRetention = time.Hour
	ctx := context.Background()
	session, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	rec := record{
		OwnerID: "owner", Request: req.Request,
		Job: protocol.Job{ID: "job_expiry", SessionID: session.ID, Status: "succeeded", FinishedAt: &finished,
			Result: &protocol.CompileResult{Success: true}},
	}
	if err := m.save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	live := m.sessions[session.ID]
	live.state.Workspace, live.state.RunningJobID = "reuse", rec.Job.ID
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.tar.gz")
	if err := os.WriteFile(checkpoint, []byte("bounded checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	executed := execution{sessionCheckpoint: true, checkpoint: checkpoint, cache: &protocol.CompileCache{}}
	executed.output.Result.Success = true
	m.publishExecution(ctx, rec, nil, &executed)
	if executed.output.Result.AuxiliaryExpiresAt == nil {
		t.Fatal("checkpoint expiry was not published")
	}
	expected := *executed.output.Result.AuxiliaryExpiresAt
	m.updatePublishedCache(ctx, rec, executed)
	m.endPublication(rec.Job.ID)
	m.admissionMu.Lock()
	m.clearSessionCacheLocked(live)
	m.admissionMu.Unlock()
	job, err := m.Get(ctx, "owner", rec.Job.ID)
	if err != nil || job.Result.AuxiliaryExpiresAt == nil || !job.Result.AuxiliaryExpiresAt.Equal(expected) {
		t.Fatalf("clearing checkpoint changed the immutable result expiry: %+v, %v", job, err)
	}
}

func TestOnlyInvalidStateDiscardsPreviousCheckpoint(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/invalid=%t", status, invalid), func(t *testing.T) {
				m, req := sessionManager(t)
				ctx := context.Background()
				session, err := m.CreateSession(ctx, "owner", req)
				if err != nil {
					t.Fatal(err)
				}
				data := []byte("previous verified checkpoint")
				source := filepath.Join(t.TempDir(), "checkpoint.tar.gz")
				if err := os.WriteFile(source, data, 0600); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				publication, err := m.projects.StageLiveCache(
					"owner",
					session.ID,
					source,
					int64(len(data)),
					hex.EncodeToString(digest[:]),
				)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = publication.Close() }()
				if err := publication.Commit(); err != nil {
					t.Fatal(err)
				}
				live := m.sessions[session.ID]
				live.cachePath, live.cacheExpires = publication.Path(), time.Now().Add(time.Hour)
				live.state.LastSuccessfulJobID, live.state.RunningJobID = "job_good", "job_attempt"
				finished := time.Now().UTC()
				m.finishSession(ctx, record{
					InvalidateCheckpoint: invalid,
					Job: protocol.Job{
						ID:         "job_attempt",
						SessionID:  session.ID,
						Status:     status,
						FinishedAt: &finished,
					},
				})
				_, statErr := os.Stat(publication.Path())
				if invalid != errors.Is(statErr, os.ErrNotExist) || live.state.LastSuccessfulJobID != "job_good" ||
					live.state.RunningJobID != "" {
					t.Fatalf(
						"previous state was incorrectly discarded/preserved: invalid=%t, stat=%v, session=%+v",
						invalid,
						statErr,
						live.state,
					)
				}
			})
		}
	}
}

type completionFaultStore struct {
	*store.Postgres
	mu               sync.Mutex
	row              store.CompileJob
	fail             bool
	failFinishedRead bool
	attempted        chan struct{}
	once             sync.Once
}

func (s *completionFaultStore) CountQueuedJobs(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.Status == "queued" {
		return 1, nil
	}
	return 0, nil
}

func (s *completionFaultStore) GetJob(context.Context, string) (store.CompileJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFinishedRead && s.row.FinishedAt != nil {
		return store.CompileJob{}, errors.New("database disconnected after completion")
	}
	return s.row, nil
}

func TestConfirmedCompletionDoesNotNeedAnotherDatabaseRead(t *testing.T) {
	m, req := sessionManager(t)
	bin := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(bin, "latexmk"),
		[]byte("#!/bin/sh\ncp main.tex main.pdf\n"),
		0700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	session, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := m.SubmitRevision(ctx, "owner", session.ID, planRevision(t, m, req, "source", 0))
	if err != nil {
		t.Fatal(err)
	}
	id := m.takeSession(<-m.queue)
	rec, err := m.load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(rec.Request)
	snapshot, _ := json.Marshal(rec.Snapshot)
	m.db = &completionFaultStore{
		failFinishedRead: true,
		row: store.CompileJob{
			ID: id, OwnerID: rec.OwnerID, ProjectID: job.ProjectID, SessionID: session.ID,
			Revision: job.Revision, SnapshotID: rec.Snapshot.ID, Status: "queued",
			Request: request, SnapshotManifest: snapshot,
		},
	}
	m.run(ctx, 1, id)
	state, err := m.GetSession(ctx, "owner", session.ID)
	if err != nil || state.RunningJobID != "" || state.LastSuccessfulJobID != id {
		t.Fatalf("confirmed durable completion left the session occupied: %+v, %v", state, err)
	}
}

func (s *completionFaultStore) TransitionJob(
	_ context.Context,
	_ string,
	expected string,
	values map[string]any,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected == "running" && s.fail {
		s.once.Do(func() { close(s.attempted) })
		return false, errors.New("database unavailable")
	}
	if s.row.Status != expected {
		return false, nil
	}
	s.row.Status = values["status"].(string)
	s.row.Error = values["error"].(string)
	s.row.Result = values["result"].([]byte)
	s.row.StartedAt, _ = values["started_at"].(*time.Time)
	s.row.FinishedAt, _ = values["finished_at"].(*time.Time)
	if s.row.FinishedAt != nil {
		s.row.SnapshotManifest = nil
	}
	return true, nil
}

func TestCompletionOutageDoesNotBlockSessionReadsAndRecovers(t *testing.T) {
	for _, cancelPending := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "cancelled"}[cancelPending], func(t *testing.T) {
			m, req := sessionManager(t)
			m.cfg.ShutdownTimeout = 150 * time.Millisecond
			bin := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(bin, "latexmk"),
				[]byte("#!/bin/sh\ncp main.tex main.pdf\nprintf 'INPUT main.tex\\nOUTPUT main.pdf\\n' > main.fls\n"),
				0700,
			); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx := context.Background()
			session, err := m.CreateSession(ctx, "owner", req)
			if err != nil {
				t.Fatal(err)
			}
			job, err := m.SubmitRevision(ctx, "owner", session.ID, planRevision(t, m, req, "source", 0))
			if err != nil {
				t.Fatal(err)
			}
			id := m.takeSession(<-m.queue)
			rec, err := m.load(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			request, _ := json.Marshal(rec.Request)
			snapshot, _ := json.Marshal(rec.Snapshot)
			fault := &completionFaultStore{
				fail:      true,
				attempted: make(chan struct{}),
				row: store.CompileJob{
					ID:               id,
					OwnerID:          rec.OwnerID,
					ProjectID:        job.ProjectID,
					SessionID:        session.ID,
					Revision:         job.Revision,
					SnapshotID:       rec.Snapshot.ID,
					Status:           "queued",
					Request:          request,
					SnapshotManifest: snapshot,
				},
			}
			m.db = fault
			done := make(chan struct{})
			go func() { m.run(ctx, 1, id); close(done) }()
			select {
			case <-fault.attempted:
			case <-time.After(3 * time.Second):
				t.Fatal("completion fault was not reached")
			}
			read := make(chan error, 1)
			go func() { _, err := m.GetSession(ctx, "owner", session.ID); read <- err }()
			select {
			case err := <-read:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("persistence retry held the admission lock")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("persistence retry occupied the worker indefinitely")
			}
			state, err := m.GetSession(ctx, "owner", session.ID)
			if err != nil || state.RunningJobID != id || state.LastSuccessfulJobID != "" {
				t.Fatalf("outage prematurely completed session: %+v, %v", state, err)
			}
			fault.mu.Lock()
			fault.fail = false
			fault.mu.Unlock()
			if cancelPending {
				if _, err := m.Cancel(ctx, "owner", id); err != nil {
					t.Fatal(err)
				}
			}
			m.retryCompletions(ctx)
			state, err = m.GetSession(ctx, "owner", session.ID)
			if err != nil || state.RunningJobID != "" {
				t.Fatalf("completion not recovered: %+v, %v", state, err)
			}
			if (state.LastSuccessfulJobID == id) == cancelPending {
				t.Fatalf("incorrect successful publication after recovery: %+v", state)
			}
			m.mu.Lock()
			pending := len(m.completions)
			m.mu.Unlock()
			if pending != 0 {
				t.Fatal("recovered completion was retained")
			}
		})
	}
}
