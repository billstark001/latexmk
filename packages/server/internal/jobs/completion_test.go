package jobs

import (
	"context"
	"encoding/json"
	"errors"
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

type completionFaultStore struct {
	*store.Postgres
	mu        sync.Mutex
	row       store.CompileJob
	fail      bool
	attempted chan struct{}
	once      sync.Once
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
	return s.row, nil
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
