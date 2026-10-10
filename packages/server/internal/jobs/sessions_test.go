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
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func sessionManager(t *testing.T) (*Manager, protocol.SessionRequest) {
	t.Helper()
	cfg := config.Config{
		StateDir:                    t.TempDir(),
		TempDir:                     t.TempDir(),
		Engines:                     []string{"xelatex"},
		MaxFiles:                    100,
		MaxExpandedBytes:            8192,
		MaxUploadBytes:              8192,
		MaxStateBytes:               1 << 20,
		MaxArtifactBytes:            8192,
		MaxLogBytes:                 8192,
		MaxConcurrentCompiles:       2,
		MaxQueuedJobs:               2,
		CompileTimeout:              3 * time.Second,
		ShutdownTimeout:             time.Second,
		MaxRealtimeSessions:         4,
		MaxRealtimeSessionsPerOwner: 4,
		RealtimeSessionTTL:          time.Minute,
	}
	p, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg, protocol.Metadata{}, compile.NewRunner(cfg), p, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, protocol.SessionRequest{
		IdempotencyKey: "session-key-00000001",
		ProjectID:      "paper",
		Workspace:      "fresh",
		Request: protocol.CompileRequest{
			ProtocolVersion: 2,
			Entry:           "main.tex",
			Engine:          "xelatex",
			Interaction:     "nonstopmode",
		},
	}
}

func TestSessionSubscriptionReleaseIsIdempotent(t *testing.T) {
	m, req := sessionManager(t)
	s, err := m.CreateSession(context.Background(), "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	release, err := m.SubscribeSession("owner", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	for i := 0; i < 4; i++ {
		release, err := m.SubscribeSession("owner", s.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	if _, err := m.SubscribeSession("owner", s.ID); !errors.Is(err, ErrSessionCapacity) {
		t.Fatalf("double release bypassed subscription capacity: %v", err)
	}
}

func TestRevisionRateDoesNotOverflowBurst(t *testing.T) {
	m, _ := sessionManager(t)
	m.cfg.MaxRealtimeRevisionRate = int(^uint(0) >> 1)
	if !m.allowRevisionLocked("owner") {
		t.Fatal("a large positive rate overflowed into an exhausted budget")
	}
}

func planRevision(
	t *testing.T,
	m *Manager,
	req protocol.SessionRequest,
	content string,
	base uint64,
) protocol.RevisionRequest {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	plan, err := m.projects.Plan(
		"owner",
		protocol.UploadPlanRequest{
			ProjectID: req.ProjectID,
			Request:   req.Request,
			Files:     []protocol.ProjectFile{{Path: "main.tex", SHA256: hash, Size: int64(len(content))}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, missing := range plan.Missing {
		if err := m.projects.PutBlob("owner", plan.UploadID, missing, bytes.NewBufferString(content)); err != nil {
			t.Fatal(err)
		}
	}
	return protocol.RevisionRequest{
		UploadID:       plan.UploadID,
		BaseRevision:   base,
		IdempotencyKey: fmt.Sprintf("revision-key-%016d", base),
	}
}

func TestSessionCoalescesWithoutGrowingQueueAndReplaysReceipts(t *testing.T) {
	m, req := sessionManager(t)
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	var last protocol.Job
	var submitted protocol.RevisionRequest
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

func TestSessionCreationReplaysBeforeQuotaAndRejectsChangedPayload(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.MaxRealtimeSessionsPerOwner = 1
	ctx := context.Background()
	first, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		replayed, err := m.CreateSession(ctx, "owner", req)
		if err != nil || replayed.ID != first.ID || len(m.sessions) != 1 {
			t.Fatalf("ambiguous creation replay = %+v, %v; sessions=%d", replayed, err, len(m.sessions))
		}
	}
	changed := req
	changed.Request.JobName = "different"
	if _, err := m.CreateSession(
		ctx,
		"owner",
		changed,
	); err == nil ||
		!strings.Contains(err.Error(), "different session") {
		t.Fatalf("changed creation payload = %v", err)
	}
	foreign, err := m.CreateSession(ctx, "other", req)
	if err != nil || foreign.ID == first.ID {
		t.Fatalf("creation key crossed owner boundary: %+v, %v", foreign, err)
	}
	req.IdempotencyKey = ""
	if _, err := m.CreateSession(ctx, "owner", req); err == nil {
		t.Fatal("creation without a replay key was accepted")
	}
}

func TestSessionRejectsControlIdempotencyKeys(t *testing.T) {
	m, req := sessionManager(t)
	s, err := m.CreateSession(context.Background(), "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"\x1b", "\x00", "\x7f", "\u0085"} {
		_, err := m.SubmitRevision(context.Background(), "owner", s.ID, protocol.RevisionRequest{
			IdempotencyKey: "revision-key-0001" + control,
		})
		if err == nil || !strings.Contains(err.Error(), "control characters") {
			t.Fatalf("control %q accepted: %v", control, err)
		}
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

func TestOnlyExplicitClientActivityRenewsSessionLease(t *testing.T) {
	m, req := sessionManager(t)
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	live := m.sessions[s.ID]
	original := time.Now().Add(10 * time.Second)
	live.state.ExpiresAt = original
	if _, err := m.GetSession(ctx, "owner", s.ID); err != nil {
		t.Fatal(err)
	}
	release, err := m.SubscribeSession("owner", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := m.SessionEvents("owner", s.ID, 0); err != nil {
		t.Fatal(err)
	}
	if !live.state.ExpiresAt.Equal(original) {
		t.Fatal("read-only status/subscription renewed the lease")
	}
	if _, err := m.RenewSession(ctx, "other", s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("foreign renewal: %v", err)
	}
	renewed, err := m.RenewSession(ctx, "owner", s.ID)
	if err != nil || !renewed.ExpiresAt.After(original) {
		t.Fatalf("explicit renewal: %+v, %v", renewed, err)
	}
	live.state.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := m.RenewSession(ctx, "owner", s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("renewal resurrected expired session: %v", err)
	}
	if len(m.sessions) != 0 {
		t.Fatal("expired subscribed session retained a slot")
	}
}

func TestSessionOwnerQuotaAndAlreadyCancelledPendingClosure(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.MaxRealtimeSessionsPerOwner = 1
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	req.IdempotencyKey = "session-key-00000002"
	if _, err := m.CreateSession(ctx, "owner", req); !errors.Is(err, ErrSessionCapacity) {
		t.Fatalf("quota=%v", err)
	}
	if _, err := m.CreateSession(ctx, "other", req); err != nil {
		t.Fatal(err)
	}
	job, err := m.SubmitRevision(ctx, "owner", s.ID, planRevision(t, m, req, "source", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel(ctx, "owner", job.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseSession(ctx, "owner", s.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSessionBoundsSubscribersAndExpiresCheckpoint(t *testing.T) {
	m, req := sessionManager(t)
	s, err := m.CreateSession(context.Background(), "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	var releases []func()
	for i := 0; i < 4; i++ {
		release, err := m.SubscribeSession("owner", s.ID)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := m.SubscribeSession("owner", s.ID); !errors.Is(err, ErrSessionCapacity) {
		t.Fatal("unbounded subscribers")
	}
	for _, release := range releases {
		release()
	}
	live := m.sessions[s.ID]
	live.cachePath = "expired-checkpoint"
	live.cacheExpires = time.Now().Add(-time.Second)
	if _, err := m.GetSession(context.Background(), "owner", s.ID); err != nil {
		t.Fatal(err)
	}
	if live.cachePath != "" {
		t.Fatal("expired checkpoint survived renewal")
	}
}

func TestClosedSessionCannotStartDequeuedJob(t *testing.T) {
	m, req := sessionManager(t)
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := m.SubmitRevision(ctx, "owner", s.ID, planRevision(t, m, req, "source", 0))
	if err != nil {
		t.Fatal(err)
	}
	id := m.takeSession(<-m.queue)
	if err := m.CloseSession(ctx, "owner", s.ID); err != nil {
		t.Fatal(err)
	}
	m.run(ctx, 1, id)
	state, err := m.Get(ctx, "owner", job.ID)
	if err != nil || state.Status != "cancelled" || state.StartedAt != nil {
		t.Fatalf("closed attempt started: %+v %v", state, err)
	}
}

func TestRevisionRateIsSharedAcrossOwnerSessionsAndReceiptsRemainReplayable(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.MaxRealtimeRevisionRate = 1
	ctx := context.Background()
	s, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	payload := planRevision(t, m, req, "first", 0)
	first, err := m.SubmitRevision(ctx, "owner", s.ID, payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.SubmitRevision(ctx, "owner", s.ID, planRevision(t, m, req, "second", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SubmitRevision(ctx, "owner", s.ID, payload); err != nil {
		t.Fatal("receipt consumed rate budget:", err)
	}
	req.IdempotencyKey = "session-key-00000002"
	other, err := m.CreateSession(ctx, "owner", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SubmitRevision(
		ctx,
		"owner",
		other.ID,
		planRevision(t, m, req, "third", 0),
	); !errors.Is(
		err,
		ErrRevisionRate,
	) {
		t.Fatalf("owner rate was bypassed: %v", err)
	}
	rec, err := m.load(ctx, first.ID)
	if err != nil || rec.Job.Status != "cancelled" || len(rec.Snapshot.Files) != 0 {
		t.Fatal("superseded job retained heavy snapshot metadata")
	}
	if _, err := m.Cancel(ctx, "owner", second.ID); err != nil {
		t.Fatal(err)
	}
	m.cfg.MaxRealtimeRevisionRate = 0
	m.cfg.MaxQueuedJobs = 1
	if _, err := m.SubmitRevision(ctx, "owner", other.ID, planRevision(t, m, req, "other", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SubmitRevision(
		ctx,
		"owner",
		s.ID,
		planRevision(t, m, req, "over-limit", 2),
	); !errors.Is(
		err,
		ErrQueueCapacity,
	) {
		t.Fatalf("cancelled pending bypassed queue limit: %v", err)
	}
}

func TestConfiguredRunnerNeverFallsBackToNativeForOrdinaryJobs(t *testing.T) {
	m, req := sessionManager(t)
	m.cfg.RunnerImage = "test@sha256:" + strings.Repeat("a", 64)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bin, "latexmk"),
		[]byte("#!/bin/sh\nprintf native > main.pdf\n"),
		0700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	snapshot := commitTestSnapshot(t, m.projects, req.Request, []byte("source"))
	job, err := m.Enqueue(context.Background(), snapshot.OwnerID, snapshot, req.Request)
	if err != nil {
		t.Fatal(err)
	}
	m.run(context.Background(), 1, job.ID)
	state, err := m.Get(context.Background(), snapshot.OwnerID, job.ID)
	if err != nil || state.Status != "failed" {
		t.Fatalf("native fallback executed on controller: %+v %v", state, err)
	}
}
