package jobs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/project"
)

func TestQueuedCompileCacheLifecycle(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
if grep -q FAIL main.tex; then
  echo corrupt > main.aux
  exit 1
fi
if test -f main.aux; then
  if grep -q COLDONLY main.tex; then exit 3; fi
  test "$(cat main.aux)" = good || exit 2
  echo warm > main.pdf
else
  echo cold > main.pdf
fi
echo good > main.aux
printf 'INPUT main.tex\nOUTPUT main.aux\nOUTPUT main.pdf\n' > main.fls
`
	if err := os.WriteFile(filepath.Join(bin, "latexmk"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Config{
		StateDir:              t.TempDir(),
		TempDir:               t.TempDir(),
		Engines:               []string{"xelatex"},
		MaxFiles:              100,
		MaxUploadBytes:        4096,
		MaxExpandedBytes:      4096,
		MaxArtifactBytes:      4096,
		MaxConcurrentCompiles: 1,
		MaxQueuedJobs:         20,
		MaxStateBytes:         1 << 20,
		MaxLogBytes:           4096,
		CompileTimeout:        5 * time.Second,
		ShutdownTimeout:       time.Second,
		CompileCacheRetention: time.Hour,
		MaxCompileCacheBytes:  4096,
	}
	projects, err := project.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg, api.Metadata{}, compile.NewRunner(cfg), projects, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := api.CompileRequest{
		ProtocolVersion: api.ProtocolVersion,
		Entry:           "main.tex",
		Engine:          "xelatex",
		Interaction:     "nonstopmode",
		Auxiliary:       api.AuxiliaryOptions{Server: "reuse"},
	}
	run := func(content string, request api.CompileRequest) api.Job {
		t.Helper()
		snapshot := commitTestSnapshot(t, projects, request, []byte(content))
		job, err := m.Enqueue(context.Background(), "member", snapshot, request)
		if err != nil {
			t.Fatal(err)
		}
		m.run(context.Background(), 1, job.ID)
		job, err = m.Get(context.Background(), "member", job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Result == nil {
			t.Fatalf("no result: %+v", job)
		}
		return job
	}
	first := run("source", req)
	if first.Status != "succeeded" || first.Result.CompileCache.Status != "miss" ||
		first.Result.CompileCache.StoredFiles != 1 {
		t.Fatalf("first %+v %+v", first, first.Result.CompileCache)
	}
	second := run("edited source", req)
	if second.Status != "succeeded" || second.Result.CompileCache.Status != "hit" ||
		second.Result.CompileCache.RestoredFiles != 1 {
		t.Fatalf("second %+v", second)
	}
	failed := run("FAIL", req)
	if failed.Status != "failed" || failed.Result.CompileCache.StoredFiles != 0 {
		t.Fatalf("failed %+v", failed)
	}
	recovered := run("fixed source", req)
	if recovered.Status != "succeeded" || recovered.Result.CompileCache.Status != "hit" {
		t.Fatal("failure corrupted last successful cache")
	}
	forced := req
	forced.Force = true
	if job := run(
		"fixed source",
		forced,
	); job.Result.CompileCache.Status != "bypass" ||
		job.Result.CompileCache.StoredFiles != 1 {
		t.Fatal("force did not rebuild cache")
	}
	retry := run("COLDONLY", req)
	if retry.Status != "succeeded" || !retry.Result.CompileCache.ColdRetry {
		t.Fatal("warm failure did not retry cold")
	}
	disabled := req
	disabled.Auxiliary.Server = "none"
	if job := run("source", disabled); job.Result.CompileCache != nil {
		t.Fatal("disabled request used cache")
	}
	preview, err := m.CleanupProject(context.Background(), "member", "paper", "cache")
	if err != nil {
		t.Fatal(err)
	}
	if preview.CompileCaches != 1 || preview.CompileCacheBytes == 0 {
		t.Fatalf("preview %+v", preview)
	}
	if _, err := m.CleanupProjectWithPlan(
		context.Background(),
		"member",
		"paper",
		"cache",
		preview.PlanDigest,
	); err != nil {
		t.Fatal(err)
	}
	if job := run("source", req); job.Result.CompileCache.Status != "miss" {
		t.Fatal("cleanup did not evict cache")
	}
}
