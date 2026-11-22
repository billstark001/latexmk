package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	shared "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestResourceSelectionAddsAndRemovesAfterTargetDefaults(t *testing.T) {
	opts := optionsFromConfig(
		config.Resolved{
			DefaultResources: []string{"default"},
			Targets: map[string]config.Target{
				"paper": {Entry: "main.tex", Resources: []string{"body", "algorithm"}},
			},
		},
		false,
	)
	opts.timeout = time.Minute
	opts.target = "paper"
	args := []string{
		"--resource",
		"extra",
		"--without-resource",
		"algorithm",
		"--frozen-lockfile",
		"--lockfile",
		"resources.json",
		"--prepare-timeout",
		"2m",
	}
	if err := parseCompileArgs(args, &opts); err != nil {
		t.Fatal(err)
	}
	if err := applyBuildTarget(
		&opts,
		config.Resolved{
			Targets: map[string]config.Target{"paper": {Entry: "main.tex", Resources: []string{"body", "algorithm"}}},
		},
		args,
	); err != nil {
		t.Fatal(err)
	}
	finalizeResourceOptions(&opts)
	if strings.Join(opts.resourceIDs, ",") != "body,extra" || opts.resourceOptions.LockMode != "frozen" ||
		opts.resourceOptions.LockFile != "resources.json" ||
		resourcePrepareTimeout(opts) != 2*time.Minute {
		t.Fatalf("unexpected resolution: %#v", opts)
	}
}

func TestFilesWithResourcesIsOfflineAndDoesNotCreateLock(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll("private/fonts", 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"main.tex":               `\documentclass{article}\usepackage{fontspec}\setmainfont{Body.otf}[Path=fonts/]\begin{document}x\end{document}`,
		"private/fonts/Body.otf": "font",
		".latexmk.json":          `{"token":{"file":"missing-token"},"uploadMode":"all","resources":{"body":{"type":"files","path":"private/fonts","mount":"fonts"}},"defaultResources":["body"]}`,
	}
	for name, value := range files {
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := captureCommandOutput(
		t,
		func() int { return run([]string{"latexmk", "files", "--json", "main.tex"}) },
	)
	if code != 0 {
		t.Fatalf("files opened credentials or failed discovery: %d %s %s", code, stdout, stderr)
	}
	var view manifestView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Files) != 1 || view.Files[0].Path != "main.tex" {
		t.Fatalf("resource source leaked: %s", stdout)
	}
	for _, name := range []string{cliresources.DefaultLockFile, cliresources.DefaultLockFile + ".guard", ".latexmk-cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("offline files created %s: %v", name, err)
		}
	}
}

func TestLockedEnvironmentPinsBaselineAndFrozenRejectsChangedSpecOffline(t *testing.T) {
	root := t.TempDir()
	defaultID := "slim-v1"
	calls, prepares := 0, 0
	resolved := make(map[string]shared.Environment)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/prepare") {
			digest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/environments/"), "/prepare")
			environment, ok := resolved[digest]
			if !ok {
				http.NotFound(w, r)
				return
			}
			environment.Ready = true
			prepares++
			if err := json.NewEncoder(w).Encode(environment); err != nil {
				t.Error(err)
			}
			return
		}
		var req shared.ResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id := req.Baseline
		if id == "" {
			id = defaultID
		}
		baselineDigest, _ := shared.Hash(id)
		environment := shared.Environment{
			Baseline:  shared.Baseline{ID: id, Digest: baselineDigest, Engines: []string{"xelatex"}},
			Resources: req.Resources,
		}
		environment.Digest, _ = shared.EnvironmentDigest(environment)
		resolved[environment.Digest] = environment
		if err := json.NewEncoder(w).Encode(environment); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, err := client.New(server.URL, "", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{projectRoot: root, target: "paper", engine: "xelatex"}
	meta := protocol.Metadata{Capabilities: protocol.Capabilities{ResourceEnvironments: true}}
	req := shared.ResolveRequest{
		Resources: []shared.Resource{{ID: "algorithm", Type: "texlive", Packages: []string{"algorithms"}}},
	}
	first, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		nil,
		"target:paper",
		req,
		resourcePrepare,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Ready || prepares != 1 {
		t.Fatal("initial environment was not prepared")
	}
	defaultID = "slim-v2"
	opts.resourceOptions.LockMode = "frozen"
	second, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		nil,
		"target:paper",
		req,
		resourcePrepare,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Ready || prepares != 2 {
		t.Fatal("locked environment was not prepared")
	}
	if first.Digest != second.Digest || second.Baseline.ID != "slim-v1" {
		t.Fatal("locked baseline drifted")
	}
	req.Resources[0].Packages = []string{"algorithm2e"}
	before := calls
	if _, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		nil,
		"target:paper",
		req,
		resourcePrepare,
	); err == nil {
		t.Fatal("stale frozen lock accepted")
	}
	if calls != before {
		t.Fatal("stale frozen lock contacted server")
	}
	// The explicit refresh operation selects the current default without changing
	// the caller's frozen compilation policy or first preparing resource bytes.
	refreshed, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		nil,
		"target:paper",
		req,
		resourceRefreshLock,
	)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Ready || prepares != 2 {
		t.Fatal("lock refresh unexpectedly prepared resource bytes")
	}
	if refreshed.Baseline.ID != "slim-v2" || opts.resourceOptions.LockMode != "frozen" {
		t.Fatal("explicit refresh did not preserve compilation policy")
	}
	updated, err := cliresources.LoadLock(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Plans["target:paper"].Environment.Digest != refreshed.Digest {
		t.Fatal("refreshed plan was not published")
	}
}

func TestFailedAutoPreparationPreservesLockAndNeverStoresLocalResourceBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "private/original"), 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "private/original/Body.otf")
	privateBytes := "private-font-contents-not-lock-metadata"
	if err := os.WriteFile(source, []byte(privateBytes), 0600); err != nil {
		t.Fatal(err)
	}
	specs := map[string]shared.Spec{"body": {Type: "files", Kind: "font", Path: "private/original", Mount: "fonts"}}
	first, err := cliresources.Capture(context.Background(), root, specs, []string{"body"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prepare") {
			http.Error(w, "preparation unavailable", http.StatusServiceUnavailable)
			return
		}
		var req shared.ResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		baselineDigest, _ := shared.Hash("slim-v1")
		environment := shared.Environment{
			Baseline:  shared.Baseline{ID: "slim-v1", Digest: baselineDigest, Engines: []string{"xelatex"}},
			Resources: req.Resources,
		}
		environment.Digest, _ = shared.EnvironmentDigest(environment)
		if err := json.NewEncoder(w).Encode(environment); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, err := client.New(server.URL, "", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{projectRoot: root, target: "paper", engine: "xelatex"}
	meta := protocol.Metadata{Capabilities: protocol.Capabilities{ResourceEnvironments: true}}
	if _, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		first,
		"target:paper",
		shared.ResolveRequest{Resources: first.Resources},
		resourceRefreshLock,
	); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, cliresources.DefaultLockFile)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), privateBytes) || strings.Contains(string(before), "private/original") ||
		strings.Contains(string(before), source) {
		t.Fatal("local resource data or source location leaked into lock")
	}
	if err := os.WriteFile(source, []byte("changed private font"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := cliresources.Capture(context.Background(), root, specs, []string{"body"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = changed.Close() }()
	if _, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		changed,
		"target:paper",
		shared.ResolveRequest{Resources: changed.Resources},
		resourcePrepare,
	); err == nil {
		t.Fatal("failed preparation accepted")
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed auto preparation replaced the old lock")
	}
	opts.resourceOptions.LockMode = "frozen"
	if _, err := resolveResourceEnvironment(
		context.Background(),
		c,
		opts,
		meta,
		changed,
		"target:paper",
		shared.ResolveRequest{Resources: changed.Resources},
		resourcePrepare,
	); err == nil {
		t.Fatal("stale frozen lock accepted")
	}
	after, err = os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("frozen rejection replaced the old lock")
	}
}

func TestEnvironmentResourceFilesRejectsInvalidOrChangedDestinations(t *testing.T) {
	invalid := shared.Environment{
		Resources: []shared.Resource{
			{
				ID:    "font",
				Type:  "files",
				Mount: "../outside",
				Files: []shared.File{{Path: "Body.otf", SHA256: strings.Repeat("a", 64)}},
			},
		},
	}
	if _, err := environmentResourceFiles(invalid, nil); err == nil {
		t.Fatal("invalid environment path was silently dropped")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Body.otf"), []byte("local font"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := cliresources.Capture(
		context.Background(),
		root,
		map[string]shared.Spec{"font": {Type: "files", Path: "Body.otf", Mount: "fonts"}},
		[]string{"font"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	changed := shared.Environment{
		Resources: []shared.Resource{
			{
				ID:    "font",
				Type:  "files",
				Mount: "fonts",
				Files: []shared.File{{Path: "Body.otf", SHA256: strings.Repeat("a", 64), Size: 10}},
			},
		},
	}
	if _, err := environmentResourceFiles(changed, captured); err == nil {
		t.Fatal("local capture with different environment bytes was reused")
	}
}
