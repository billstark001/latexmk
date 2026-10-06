package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	projectarchive "github.com/billstark001/latexmk/packages/server/internal/archive"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/resultarchive"
)

func workerInput(t *testing.T, root, source, checkpoint string) []byte {
	t.Helper()
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "main.tex"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := describeFile(staging, "main.tex", 4096)
	if err != nil {
		t.Fatal(err)
	}
	req := workerRequest{
		Version: 1,
		Request: api.CompileRequest{
			ProtocolVersion: 2,
			Entry:           "main.tex",
			Engine:          "xelatex",
			Interaction:     "nonstopmode",
			RecordInputs:    true,
		},
		RequestID:        "job-test",
		Sources:          []api.ProjectFile{{Path: "main.tex", Size: file.Size, SHA256: file.SHA256}},
		MaxFiles:         100,
		MaxSourceBytes:   8192,
		MaxStateBytes:    8192,
		MaxArtifactBytes: 8192,
		MaxLogBytes:      8192,
		TimeoutMS:        5000,
		Warm:             checkpoint != "",
	}
	raw, _ := json.Marshal(req)
	if err := os.WriteFile(filepath.Join(staging, "request.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	meta, err := describeFile(staging, "request.json", 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	members := []archiveMember{
		{name: "request.json", file: meta},
		{name: "sources/main.tex", file: file, modTime: time.Unix(946684800, 0)},
	}
	if checkpoint != "" {
		cached, err := describeFile(filepath.Dir(checkpoint), filepath.Base(checkpoint), 8192)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, archiveMember{name: "checkpoint.tar.gz", file: cached})
	}
	var input bytes.Buffer
	if err := writeArchive(&input, members); err != nil {
		t.Fatal(err)
	}
	return input.Bytes()
}

func TestWorkerCheckpointPreservesNoopResultsAndRejectsFailedState(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
if grep -q FAIL main.tex; then
  echo poisoned > .latexmk-build/main.aux
  exit 1
fi
if test -f .latexmk-build/main.fdb_latexmk; then
  echo 'Nothing to do'
  exit 0
fi
cat main.tex > .latexmk-build/main.pdf
echo aux > .latexmk-build/main.aux
echo dependencies > .latexmk-build/main.fdb_latexmk
printf 'PWD %s\nINPUT main.tex\nOUTPUT .latexmk-build/main.pdf\nOUTPUT .latexmk-build/main.aux\n' "$PWD" > .latexmk-build/main.fls
`
	if err := os.WriteFile(filepath.Join(bin, "latexmk"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Fixed root is reused across attempts, matching the container's logical path.
	root := t.TempDir()
	cleanup := func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
		_ = os.RemoveAll(root)
		_ = os.MkdirAll(root, 0700)
	}
	t.Cleanup(cleanup)
	run := func(source, checkpoint string) (bool, string, string) {
		t.Helper()
		input := workerInput(t, root, source, checkpoint)
		cleanup()
		var response bytes.Buffer
		if err := runWorker(context.Background(), bytes.NewReader(input), &response, root, 1<<20, 200); err != nil {
			t.Fatal(err)
		}
		dest := t.TempDir()
		if _, err := projectarchive.ExtractTarGz(
			bytes.NewReader(response.Bytes()),
			dest,
			projectarchive.Limits{MaxFiles: 2, MaxBytes: 1 << 20},
		); err != nil {
			t.Fatal(err)
		}
		artifactRoot := t.TempDir()
		encoded, err := os.Open(filepath.Join(dest, "result.tar.gz"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := resultarchive.Decode(
			encoded,
			artifactRoot,
			resultarchive.Limits{MaxFiles: 100, MaxArtifacts: 8192, MaxLogs: 8192},
		)
		_ = encoded.Close()
		if err != nil {
			t.Fatal(err)
		}
		pdf := ""
		if data, err := os.ReadFile(filepath.Join(artifactRoot, "artifacts", "main.pdf")); err == nil {
			pdf = string(data)
		}
		return result.Result.Success, filepath.Join(dest, "checkpoint.tar.gz"), pdf
	}
	ok, checkpoint, pdf := run("source", "")
	if !ok || pdf != "source" {
		t.Fatalf("cold result %v %q", ok, pdf)
	}
	ok, _, pdf = run("source", checkpoint)
	if !ok || pdf != "source" {
		t.Fatalf("noop lost output %v %q", ok, pdf)
	}
	ok, failed, _ := run("FAIL", checkpoint)
	if ok {
		t.Fatal("failed TeX succeeded")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatal("failed workspace published a checkpoint")
	}
}

func TestCheckpointRejectsSymlinksAndEnforcesTotalBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state"), bytes.Repeat([]byte("x"), 100), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectState(root, 100, 50); err == nil {
		t.Fatal("checkpoint exceeded byte limit")
	}
	if err := os.Symlink("state", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectState(root, 100, 4096); err == nil {
		t.Fatal("checkpoint accepted a symlink")
	}
}

func TestContainerArgumentsEnforceIsolation(t *testing.T) {
	args := strings.Join(containerArgs(testConfig(), "attempt"), " ")
	for _, flag := range []string{"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--memory-swap", "--pids-limit", "noexec", "nr_inodes=", "--pull=never"} {
		if !strings.Contains(args, flag) {
			t.Fatalf("missing %s", flag)
		}
	}
	if strings.Contains(args, "--volume") || strings.Contains(args, "--mount") ||
		strings.Contains(args, "--privileged") {
		t.Fatal("runner exposes host state")
	}
}

func testConfig() config.Config {
	return config.Config{
		RunnerImage:          "image@sha256:" + strings.Repeat("a", 64),
		RunnerNamespace:      "test-runner",
		RunnerWorkspaceBytes: 1 << 20,
		RunnerMemoryBytes:    1 << 28,
		RunnerPIDs:           16,
		RunnerCPUs:           1,
		MaxFiles:             100,
	}
}
