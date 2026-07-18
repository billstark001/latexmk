package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
)

func captureCommandOutput(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	oldStdout, oldStderr := os.Stdout, os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = stdoutW, stderrW
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
	}()
	code := fn()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatal(err)
	}
	_ = stdoutR.Close()
	_ = stderrR.Close()
	return code, string(stdout), string(stderr)
}

func TestNormalizeCompilePathsResolvesProjectRootSymlink(t *testing.T) {
	physicalRoot := t.TempDir()
	entry := filepath.Join(physicalRoot, "main.tex")
	if err := os.WriteFile(entry, []byte("\\documentclass{article}"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(physicalRoot, alias); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(physicalRoot)
	if err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{projectRoot: alias, entry: "main.tex"}
	if err := normalizeCompilePaths(&opts, physicalRoot); err != nil {
		t.Fatal(err)
	}
	if opts.projectRoot != resolvedRoot {
		t.Fatalf("project root = %q, want %q", opts.projectRoot, resolvedRoot)
	}
	if opts.entry != "main.tex" {
		t.Fatalf("entry = %q, want main.tex", opts.entry)
	}
}

func TestNormalizeCompilePathsDefaultsToEntryDirectory(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "paper")
	entry := filepath.Join(project, "main.tex")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("\\documentclass{article}"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{rootMode: "entry", entry: filepath.Join("paper", "main.tex")}
	if err := normalizeCompilePaths(&opts, parent); err != nil {
		t.Fatal(err)
	}
	resolvedProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if opts.projectRoot != resolvedProject {
		t.Fatalf("project root = %q, want %q", opts.projectRoot, resolvedProject)
	}
	if opts.entry != "main.tex" {
		t.Fatalf("entry = %q, want main.tex", opts.entry)
	}
}

func TestNormalizeCompilePathsUsesGitRootOnlyWhenRequested(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(repo, "papers", "demo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(project, "main.tex")
	if err := os.WriteFile(entry, []byte("\\documentclass{article}"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{rootMode: "git", entry: "main.tex"}
	if err := normalizeCompilePaths(&opts, project); err != nil {
		t.Fatal(err)
	}
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if opts.projectRoot != resolvedRepo {
		t.Fatalf("project root = %q, want %q", opts.projectRoot, resolvedRepo)
	}
	if opts.entry != "papers/demo/main.tex" {
		t.Fatalf("entry = %q, want papers/demo/main.tex", opts.entry)
	}
}

func TestParseCompileArgsRejectsUnknownRootMode(t *testing.T) {
	opts := compileOptions{timeout: time.Minute}
	if err := parseCompileArgs([]string{"--root-mode", "parent", "main.tex"}, &opts); err == nil {
		t.Fatal("expected invalid root mode error")
	}
}

func TestParseCompileArgsReadsTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{timeout: time.Minute}
	if err := parseCompileArgs([]string{"--token-file", path, "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.token != "file-token" {
		t.Fatalf("token = %q, want file-token", opts.token)
	}
}

func TestParseCompileArgsUploadMode(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, uploadMode: "auto"}
	if err := parseCompileArgs([]string{"--upload-mode", "all", "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.uploadMode != "all" {
		t.Fatalf("upload mode = %q, want all", opts.uploadMode)
	}
	if err := parseCompileArgs([]string{"--upload-mode", "legacy", "main.tex"}, &opts); err == nil {
		t.Fatal("expected unsupported upload mode to fail")
	}
}

func TestParseCompileArgsManifestFiles(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, uploadMode: "auto"}
	args := []string{"--upload-mode", "manifest", "--manifest", ".latexmk-files", "--include-file", "chapter.tex", "--include-file=figure.pdf", "main.tex"}
	if err := parseCompileArgs(args, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.uploadMode != "manifest" || opts.manifestFile != ".latexmk-files" {
		t.Fatalf("manifest options = %#v", opts)
	}
	if len(opts.includeFiles) != 2 || opts.includeFiles[0] != "chapter.tex" || opts.includeFiles[1] != "figure.pdf" {
		t.Fatalf("include files = %#v", opts.includeFiles)
	}
}

func TestParseCompileArgsWatchOptions(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, watchInterval: 500 * time.Millisecond, watchDebounce: 500 * time.Millisecond}
	if err := parseCompileArgs([]string{"--watch", "--watch-interval", "25ms", "--watch-debounce=75ms", "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if !opts.watch || opts.watchInterval != 25*time.Millisecond || opts.watchDebounce != 75*time.Millisecond {
		t.Fatalf("watch options = %#v", opts)
	}
	opts.watchInterval = 0
	if err := parseCompileArgs([]string{"--watch", "main.tex"}, &opts); err == nil {
		t.Fatal("expected zero watch interval to fail")
	}
}

func TestParseCompileArgsDetach(t *testing.T) {
	opts := compileOptions{timeout: time.Minute}
	if err := parseCompileArgs([]string{"--detach", "--json", "main.tex"}, &opts); err != nil {
		t.Fatal(err)
	}
	if !opts.detach || !opts.jsonOutput || opts.entry != "main.tex" {
		t.Fatalf("detach options = %#v", opts)
	}
}

func TestCapabilityErrorUsesStableAgentCode(t *testing.T) {
	code, details, retryable, exitCode := classifyAgentError(&client.CapabilityError{Capability: "detached queued compilation"})
	if code != "unsupported_capability" || retryable || exitCode != 1 || details["capability"] != "detached queued compilation" {
		t.Fatalf("classification = %q %#v %t %d", code, details, retryable, exitCode)
	}
}

func TestParseResultCommandArgs(t *testing.T) {
	opts := resultCommandOptions{timeout: time.Minute, source: "all", tailLines: 200, maxBytes: 64 << 10}
	if err := parseResultCommandArgs("logs", []string{"job_test", "--source", "compiler", "--tail", "25", "--max-bytes", "4096", "--json"}, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.jobID != "job_test" || opts.source != "compiler" || opts.tailLines != 25 || opts.maxBytes != 4096 || !opts.jsonOutput {
		t.Fatalf("log options = %#v", opts)
	}
	opts = resultCommandOptions{timeout: time.Minute}
	if err := parseResultCommandArgs("artifacts.get", []string{"job_test", strings.Repeat("a", 32), "--out-dir", "build"}, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.jobID != "job_test" || opts.artifactID != strings.Repeat("a", 32) || opts.outDir != "build" {
		t.Fatalf("artifact options = %#v", opts)
	}
}

func TestResultStateErrorUsesRetryableAgentCode(t *testing.T) {
	code, details, retryable, exitCode := classifyAgentError(&client.ResultStateError{Status: "running"})
	if code != "result_not_ready" || !retryable || exitCode != 1 || details["status"] != "running" {
		t.Fatalf("classification = %q %#v %t %d", code, details, retryable, exitCode)
	}
}

func TestSelectedFilesChangedIgnoresNewRecorderDependencies(t *testing.T) {
	before := []projectarchive.File{{Path: "main.tex", SHA256: "main-v1"}}
	after := []projectarchive.File{{Path: "main.tex", SHA256: "main-v1"}, {Path: "dynamic.tex", SHA256: "dynamic-v1"}}
	if selectedFilesChanged(before, after) {
		t.Fatal("a newly discovered dependency should not imply an edit during compilation")
	}
	after[0].SHA256 = "main-v2"
	if !selectedFilesChanged(before, after) {
		t.Fatal("an existing selected file change was not detected")
	}
}

func TestWatchTargetsOnlyAddsSelectedFilesAndPolicyControls(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(repo, "paper")
	if err := os.MkdirAll(filepath.Join(project, "sections"), 0o700); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(project, "main.tex")
	bodyPath := filepath.Join(project, "sections", "body.tex")
	targets := watchTargets(compileOptions{projectRoot: project, gitIgnore: true, manifestFile: ".latexmk-files"}, []projectarchive.File{
		{Path: "main.tex", Source: mainPath},
		{Path: "sections/body.tex", Source: bodyPath},
	})
	paths := make(map[string]bool)
	for _, target := range targets {
		paths[target.Path] = true
	}
	for _, required := range []string{
		mainPath,
		bodyPath,
		filepath.Join(project, ".latexmk-files"),
		filepath.Join(project, ".gitignore"),
		filepath.Join(project, "sections", ".gitignore"),
		filepath.Join(repo, ".gitignore"),
		filepath.Join(repo, ".git", "info", "exclude"),
	} {
		if !paths[required] {
			t.Errorf("missing watch target %s", required)
		}
	}
	if paths[filepath.Join(project, "unrelated-secret.txt")] {
		t.Fatal("watcher included an unrelated project file")
	}
}
func TestJobsListJSONUsesVersionedEnvelope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LATEXMK_TOKEN", "")
	t.Setenv("LATEXMK_TOKEN_FILE", "")
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs" || r.URL.Query().Get("limit") != "2" || r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []map[string]any{
			{"id": "job_old", "projectId": "project", "status": "succeeded", "createdAt": older},
			{"id": "job_new", "projectId": "project", "status": "queued", "createdAt": newer},
		}})
	}))
	defer server.Close()
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "jobs", "list", "--server", server.URL, "--token", "secret-token", "--limit", "2", "--json"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "secret-token") {
		t.Fatal("JSON output exposed the bearer token")
	}
	var envelope struct {
		SchemaVersion int    `json:"schemaVersion"`
		OK            bool   `json:"ok"`
		Command       string `json:"command"`
		Data          struct {
			Count int `json:"count"`
			Limit int `json:"limit"`
			Jobs  []struct {
				ID string `json:"id"`
			} `json:"jobs"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || !envelope.OK || envelope.Command != "jobs.list" || envelope.Data.Count != 2 || envelope.Data.Limit != 2 || envelope.Data.Jobs[0].ID != "job_new" {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestJobsJSONErrorIsStableAndDoesNotExposeToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LATEXMK_TOKEN", "")
	t.Setenv("LATEXMK_TOKEN_FILE", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"job not found"}`))
	}))
	defer server.Close()
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "jobs", "show", "job_missing", "--server", server.URL, "--token", "secret-token", "--json"})
	})
	if code != 1 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "secret-token") {
		t.Fatal("JSON error exposed the bearer token")
	}
	var envelope agentJSONEnvelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Command != "jobs.show" || envelope.Error == nil || envelope.Error.Code != "not_found" || envelope.Error.Retryable || envelope.Error.Details["httpStatus"] != float64(http.StatusNotFound) {
		t.Fatalf("error envelope = %#v", envelope)
	}
}

func TestJobsInvalidArgumentsUseJSONError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "jobs", "list", "--limit", "0", "--json"})
	})
	if code != 2 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var envelope agentJSONEnvelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == nil || envelope.Error.Code != "invalid_arguments" || envelope.Error.Retryable {
		t.Fatalf("error envelope = %#v", envelope)
	}
}
