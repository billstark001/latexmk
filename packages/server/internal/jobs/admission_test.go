package jobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestQueueCountTracksConditionalTransitionsAndRecovery(t *testing.T) {
	m, _ := sessionManager(t)
	ctx := context.Background()
	rec := record{OwnerID: "owner", Job: protocol.Job{ID: "job_count", Status: "queued"}}
	if err := m.save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	assertCount := func(want int) {
		t.Helper()
		got, err := m.pendingCount(ctx)
		if err != nil || got != want {
			t.Fatalf("pending count = %d, %v; want %d", got, err, want)
		}
	}
	assertCount(1)
	rec.Job.Status = "running"
	if changed, err := m.transition(ctx, rec, "queued"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	assertCount(0)
	if changed, err := m.transition(ctx, rec, "queued"); err != nil || changed {
		t.Fatal(changed, err)
	}
	assertCount(0)
	now := time.Now().UTC()
	rec.Job.Status, rec.Job.FinishedAt = "succeeded", &now
	m.deferCompletion(rec)
	assertCount(1)
	m.retryCompletions(ctx)
	assertCount(0)
	m.pruneTerminal(ctx, now.Add(time.Second))
	assertCount(0)
}

var benchmarkPending int

func BenchmarkAdmissionHistory(b *testing.B) {
	for _, retained := range []int{1000, 100000} {
		m := &Manager{jobs: make(map[string]record, retained+1), queued: 1}
		for i := 0; i < retained; i++ {
			m.jobs[fmt.Sprint(i)] = record{Job: protocol.Job{Status: "succeeded"}}
		}
		m.jobs["queued"] = record{Job: protocol.Job{Status: "queued"}}
		b.Run(fmt.Sprintf("scan/%d", retained), func(b *testing.B) {
			for b.Loop() {
				m.mu.Lock()
				count := 0
				for _, rec := range m.jobs {
					if rec.Job.Status == "queued" {
						count++
					}
				}
				m.mu.Unlock()
				benchmarkPending = count
			}
		})
		b.Run(fmt.Sprintf("counter/%d", retained), func(b *testing.B) {
			for b.Loop() {
				count, err := m.pendingCount(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				benchmarkPending = count
			}
		})
	}
}
