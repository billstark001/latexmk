package jobs

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestNativeHistoryOrdersBeforeLimitingAndSurvivesCleanup(t *testing.T) {
	m, _ := sessionManager(t)
	ctx := context.Background()
	base := time.Now().UTC()
	add := func(id, owner, project, status string, offset time.Duration) {
		t.Helper()
		created := base.Add(offset)
		job := protocol.Job{ID: id, ProjectID: project, Status: status, CreatedAt: created}
		if isTerminal(status) {
			job.FinishedAt = &created
		}
		if err := m.save(ctx, record{OwnerID: owner, Job: job}); err != nil {
			t.Fatal(err)
		}
		m.endPublication(id)
	}
	// Insert out of order and include equal timestamps and another owner.
	add("job_new", "owner", "paper", "succeeded", 3*time.Second)
	add("job_old", "owner", "paper", "failed", 0)
	add("job_other", "other", "paper", "succeeded", 4*time.Second)
	add("job_a", "owner", "other-paper", "cancelled", time.Second)
	add("job_b", "owner", "other-paper", "queued", time.Second)
	assertIDs := func(limit int, want ...string) {
		t.Helper()
		jobs, err := m.List(ctx, "owner", limit)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(jobs))
		for _, job := range jobs {
			ids = append(ids, job.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("list = %v; want %v", ids, want)
		}
	}
	assertIDs(2, "job_new", "job_b")
	assertIDs(10, "job_new", "job_b", "job_a", "job_old")
	rec, err := m.load(ctx, "job_b")
	if err != nil {
		t.Fatal(err)
	}
	rec.Job.Status = "running"
	if changed, err := m.transition(ctx, rec, "queued"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	assertIDs(2, "job_new", "job_b")
	m.pruneTerminal(ctx, base.Add(2*time.Second))
	assertIDs(10, "job_new", "job_b")
	if err := m.deleteTerminalProjectRecords(ctx, "owner", "paper"); err != nil {
		t.Fatal(err)
	}
	assertIDs(10, "job_b")
	if err := m.deleteTerminalProjectRecords(ctx, "other", "paper"); err != nil {
		t.Fatal(err)
	}
	if _, present := m.jobHistory["other"]; present {
		t.Fatal("empty owner history was retained")
	}
}

var benchmarkHistory []protocol.Job

func BenchmarkNativeHistoryPage(b *testing.B) {
	for _, retained := range []int{1000, 100000} {
		m := &Manager{jobs: make(map[string]record, retained), jobHistory: make(map[string][]string)}
		base := time.Now().UTC()
		for i := 0; i < retained; i++ {
			rec := record{OwnerID: "owner", Job: protocol.Job{
				ID: fmt.Sprintf("job_%06d", i), Status: "succeeded", CreatedAt: base.Add(time.Duration(i)),
			}}
			m.jobs[rec.Job.ID] = rec
			m.indexHistoryLocked(rec)
		}
		b.Run(fmt.Sprintf("latest-50/%d", retained), func(b *testing.B) {
			for b.Loop() {
				jobs, err := m.List(context.Background(), "owner", 50)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkHistory = jobs
			}
		})
	}
}
