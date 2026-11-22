package jobs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type compileStageLog struct {
	Message           string `json:"msg"`
	JobID             string `json:"job_id"`
	ExecutionMS       *int64 `json:"execution_ms"`
	CompileRunnerMS   *int64 `json:"compile_runner_ms"`
	ExecuteOverheadMS *int64 `json:"execute_overhead_ms"`
	WorkspaceReuse    *bool  `json:"workspace_reuse"`
	ColdRetry         *bool  `json:"cold_retry"`
}

func requireCompileStageLog(t *testing.T, output []byte, job protocol.Job) compileStageLog {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var record compileStageLog
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			t.Fatalf("missing compile job stages record for %s: %s", job.ID, output)
		}
		if err != nil {
			t.Fatal(err)
		}
		if record.Message != "compile job stages" || record.JobID != job.ID {
			continue
		}
		if record.ExecutionMS == nil || record.CompileRunnerMS == nil || record.ExecuteOverheadMS == nil ||
			record.WorkspaceReuse == nil || record.ColdRetry == nil {
			t.Fatalf("stage log omitted timing or reuse fields: %s", output)
		}
		if *record.ExecuteOverheadMS < 0 ||
			*record.ExecuteOverheadMS != max(0, *record.ExecutionMS-*record.CompileRunnerMS) {
			t.Fatalf("stage log reported inconsistent execution overhead: %s", output)
		}
		coldRetry := job.Result.CompileCache != nil && job.Result.CompileCache.ColdRetry
		if *record.WorkspaceReuse != job.Result.WorkspaceReuse || *record.ColdRetry != coldRetry {
			t.Fatalf("stage log did not use the final attempt's reuse state: %s", output)
		}
		return record
	}
}

func TestStageLogUsesWorkerTimingAndWorkspaceReuseWithoutDocker(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to emulate the worker archive protocol")
	}
	bin := t.TempDir()
	// This executable replaces Docker rather than contacting a daemon. It reads
	// the real worker request and returns a valid result archive with its warm
	// state. The exaggerated inner duration exercises the nonnegative clamp.
	script := `#!` + python + `
import hashlib, io, json, sys, tarfile
if len(sys.argv) < 2 or sys.argv[1] != 'run':
    sys.exit(0)
with tarfile.open(fileobj=sys.stdin.buffer, mode='r|gz') as incoming:
    request = None
    for member in incoming:
        if member.name == 'request.json':
            request = json.load(incoming.extractfile(member))
if request is None:
    sys.exit(2)
def archive(members):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode='w:gz') as packed:
        for name, data in members:
            member = tarfile.TarInfo(name)
            member.size = len(data)
            packed.addfile(member, io.BytesIO(data))
    return output.getvalue()
source = request['request']
pdf = b'%PDF-1.4 timing fixture'
result = {
    'protocolVersion': source['protocolVersion'],
    'requestId': request['requestId'],
    'entry': source['entry'],
    'engine': source['engine'],
    'success': True,
    'exitCode': 0,
    'durationMs': 54321,
    'workspaceReuse': request['warm'],
    'inputFiles': ['main.tex'],
    'artifacts': [{'path': 'main.pdf', 'size': len(pdf),
                   'sha256': hashlib.sha256(pdf).hexdigest()}]
}
result_archive = archive([
    ('result.json', json.dumps(result).encode()),
    ('stdout.log', b''), ('stderr.log', b''), ('artifacts/main.pdf', pdf)
])
sys.stdout.buffer.write(archive([('result.tar.gz', result_archive)]))
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, request := sessionManager(t)
	manager.cfg.RunnerImage = "timing-fixture@sha256:" + strings.Repeat("a", 64)
	manager.cfg.RunnerWorkspaceBytes = 1 << 20
	manager.cfg.MaxCompileCacheBytes = 4096
	manager.cfg.CompileCacheRetention = time.Hour
	request.Workspace = "reuse"
	request.Request.Auxiliary.Server = "reuse"
	var logs bytes.Buffer
	manager.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()
	session, err := manager.CreateSession(ctx, "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.SubmitRevision(ctx, "owner", session.ID, planRevision(t, manager, request, "source", 0))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := manager.load(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.tar.gz")
	writeTimingCheckpoint(t, checkpoint)
	state := manager.sessions[session.ID]
	state.cachePath, state.cacheExpires = checkpoint, time.Now().Add(time.Hour)
	state.cacheInputs = queued.Snapshot.Files
	state.cacheHashes, state.cacheStamps = map[string]string{}, map[string]int64{}
	for _, source := range queued.Snapshot.Files {
		state.cacheHashes[source.Path] = source.SHA256
		state.cacheStamps[source.Path] = 946684800
	}
	if running := manager.takeSession(<-manager.queue); running != job.ID {
		t.Fatalf("unexpected scheduled job: %s", running)
	}
	manager.run(ctx, 1, job.ID)
	job, err = manager.Get(ctx, "owner", job.ID)
	if err != nil || job.Status != "succeeded" || job.Result == nil || !job.Result.WorkspaceReuse {
		t.Fatalf("emulated reused worker did not complete: %+v %v", job, err)
	}
	stage := requireCompileStageLog(t, logs.Bytes(), job)
	if *stage.CompileRunnerMS != 54321 || *stage.ExecuteOverheadMS != 0 || *stage.ColdRetry {
		t.Fatalf("worker timing was overwritten or reuse was inferred from retry state: %s", logs.Bytes())
	}
	if *stage.ExecutionMS != job.Result.DurationMS || *stage.ExecutionMS >= *stage.CompileRunnerMS {
		t.Fatalf("public execution timing was not kept separate from the inner duration: %s", logs.Bytes())
	}
}

func writeTimingCheckpoint(t *testing.T, destination string) {
	t.Helper()
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	packed := tar.NewWriter(compressed)
	if err := packed.WriteHeader(&tar.Header{Name: "main.aux", Mode: 0600, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(packed, "good"); err != nil {
		t.Fatal(err)
	}
	if err := packed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
