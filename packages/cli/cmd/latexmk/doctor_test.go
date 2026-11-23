package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func doctorTestMetadata() protocol.Metadata {
	return protocol.Metadata{Service: "latexmk", Version: "test", ProtocolVersion: protocol.Version,
		Capabilities: protocol.Capabilities{
			RealtimeSessions: true, ResourceEnvironments: true, CompileCache: true, AuxiliaryRetention: true,
			QueuedJobs: true, IncrementalUpload: true, MaxCompileCacheBytes: 12345,
		}}
}

func doctorTestInventory() client.EnvironmentInventory {
	return client.EnvironmentInventory{Default: "native", Baselines: []resources.Baseline{
		{ID: "native", Digest: strings.Repeat("a", 64), Engines: []string{"xelatex"}},
		{
			ID:      "image",
			Digest:  strings.Repeat("b", 64),
			Engines: []string{"xelatex"},
			Image:   "runner@sha256:" + strings.Repeat("c", 64),
		},
	}}
}

func hasDoctorReason(result doctorDiagnostics, code string) bool {
	for _, reason := range result.Realtime.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func TestDoctorSeparatesSupportedAndEnabledUsingSelectedBaseline(t *testing.T) {
	for _, test := range []struct {
		name        string
		environment string
		global      bool
		realtime    bool
		policy      string
		mutate      func(*protocol.Metadata)
		supported   bool
		enabled     bool
		reason      string
	}{
		{name: "image-overrides-native-controller", environment: "image", realtime: true, policy: "reuse", supported: true, enabled: true},
		{name: "native-overrides-image-controller", environment: "native", global: true, realtime: true, policy: "reuse", reason: "baseline-not-isolated"},
		{name: "supported-but-not-selected", environment: "image", policy: "none", supported: true, reason: "reuse-not-selected"},
		{name: "cache-selected-without-realtime", environment: "image", policy: "reuse", supported: true, reason: "realtime-not-selected"},
		{name: "realtime-fresh", environment: "image", realtime: true, policy: "none", supported: true, reason: "reuse-not-selected"},
		{name: "cache-disabled", environment: "image", realtime: true, policy: "reuse", mutate: func(m *protocol.Metadata) { m.Capabilities.CompileCache = false }, reason: "compile-cache-disabled"},
		{name: "sessions-disabled", environment: "image", realtime: true, policy: "reuse", mutate: func(m *protocol.Metadata) { m.Capabilities.RealtimeSessions = false }, reason: "sessions-disabled"},
		{name: "missing-queued-jobs", environment: "image", realtime: true, policy: "reuse", mutate: func(m *protocol.Metadata) { m.Capabilities.QueuedJobs = false }, reason: "resource-transport-unsupported"},
		{name: "missing-incremental-upload", environment: "image", realtime: true, policy: "reuse", mutate: func(m *protocol.Metadata) { m.Capabilities.IncrementalUpload = false }, reason: "resource-transport-unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			meta := doctorTestMetadata()
			meta.Capabilities.IsolatedWorkspaces = test.global
			if test.mutate != nil {
				test.mutate(&meta)
			}
			opts := compileOptions{projectRoot: t.TempDir(), engine: "xelatex", environment: test.environment,
				realtime: test.realtime, auxiliary: protocol.AuxiliaryOptions{Server: test.policy}}
			result := diagnoseRealtime(opts, meta, doctorTestInventory(), nil)
			if result.Realtime.Supported != test.supported || result.Realtime.Enabled != test.enabled {
				t.Fatalf("unexpected diagnosis: %#v", result)
			}
			if test.reason != "" && !hasDoctorReason(result, test.reason) {
				t.Fatalf("missing reason %s: %#v", test.reason, result)
			}
		})
	}
}

func TestDoctorJSONUsesTargetFlagsAndOnlyReadsResources(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	meta, inventory := doctorTestMetadata(), doctorTestInventory()
	var calls []string
	var callsMu sync.Mutex
	readCalls := func() []string {
		callsMu.Lock()
		defer callsMu.Unlock()
		return append([]string(nil), calls...)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		callsMu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("doctor mutated server state: %s", r.Method)
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		var value any
		switch r.URL.Path {
		case "/healthz":
			return
		case "/v1/meta":
			value = meta
		case "/v1/environments":
			value = inventory
		default:
			t.Errorf("unexpected doctor endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	// Neither the TeX entry nor this private font directory exists. Diagnosis
	// must still use the inventory and recorded manifests without opening them.
	config := `{"auxiliary":{"server":"none"},"defaultTarget":"paper","targets":{"paper":{"entry":"main.tex","environment":"native","resources":["body"]}},"resources":{"body":{"type":"files","kind":"font","path":"private/absent","mount":"fonts"}}}`
	if err := os.WriteFile(".latexmk.json", []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	request := resources.ResolveRequest{Baseline: "image", Resources: []resources.Resource{{
		ID: "body", Type: "files", Kind: "font", Mount: "fonts",
		Files: []resources.File{{Path: "Body.otf", SHA256: strings.Repeat("d", 64), Size: 4}},
	}}}
	plan := doctorTestPlan(t, request, inventory.Baselines[1])
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := cliresources.SavePlan(root, "", "target:paper", plan); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cliresources.DefaultLockFile)
	if err != nil {
		t.Fatal(err)
	}
	entriesBefore, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{
			"latexmk",
			"doctor",
			"--json",
			"--server",
			server.URL,
			"--token-mode",
			"none",
			"--target",
			"paper",
			"--environment",
			"image",
			"--realtime",
			"--server-cache",
			"none",
			"--server-cache",
			"reuse",
		})
	})
	if code != 0 {
		t.Fatalf("doctor code %d: %s %s", code, stdout, stderr)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Service != meta.Service || report.Version != meta.Version ||
		report.Capabilities.MaxCompileCacheBytes != 12345 {
		t.Fatalf("metadata fields were not retained: %s", stdout)
	}
	if report.Doctor.Target != "paper" || report.Doctor.Engine != "xelatex" || !report.Doctor.Realtime.Enabled ||
		report.Doctor.Realtime.Baseline.Source != "lock" || report.Doctor.Realtime.LockState != "matching-recorded-requirements" ||
		report.Doctor.Realtime.ResourceContentsChecked {
		t.Fatalf("target/CLI/lock resolution: %s", stdout)
	}
	after, err := os.ReadFile(cliresources.DefaultLockFile)
	if err != nil || string(after) != string(before) {
		t.Fatalf("doctor changed lock: %v", err)
	}
	entriesAfter, err := os.ReadDir(root)
	if err != nil || !reflect.DeepEqual(entriesBefore, entriesAfter) {
		t.Fatalf("doctor created project state: %v", err)
	}
	if !reflect.DeepEqual(readCalls(), []string{"GET /healthz", "GET /v1/meta", "GET /v1/environments"}) {
		t.Fatalf("unexpected server calls: %v", readCalls())
	}
	// meta remains the original metadata response, without doctor diagnostics,
	// target selection, health check or inventory calls.
	callsMu.Lock()
	calls = nil
	callsMu.Unlock()
	code, stdout, stderr = captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "meta", "--json", "--server", server.URL, "--token-mode", "none"})
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatal(err)
	}
	if code != 0 || raw["doctor"] != nil || !reflect.DeepEqual(readCalls(), []string{"GET /v1/meta"}) {
		t.Fatalf("meta shape/behavior changed: %d %s %s calls=%v", code, stdout, stderr, readCalls())
	}
}

func doctorTestPlan(t *testing.T, request resources.ResolveRequest, baseline resources.Baseline) resources.LockedPlan {
	t.Helper()
	normalized, err := resources.NormalizeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := resources.Hash(normalized)
	if err != nil {
		t.Fatal(err)
	}
	environment := resources.Environment{
		Baseline:  baseline,
		Resources: normalized.Resources,
		Requires:  normalized.Requires,
	}
	environment.Digest, err = resources.EnvironmentDigest(environment)
	if err != nil {
		t.Fatal(err)
	}
	return resources.LockedPlan{SpecDigest: digest, Environment: environment}
}

func TestDoctorReportsFrozenLocksAndUnavailableInventory(t *testing.T) {
	root := t.TempDir()
	opts := compileOptions{projectRoot: root, target: "paper", engine: "xelatex", realtime: true,
		environment: "image", auxiliary: protocol.AuxiliaryOptions{Server: "reuse"},
		resourceOptions: resources.Options{LockMode: "frozen"}}
	meta, inventory := doctorTestMetadata(), doctorTestInventory()
	result := diagnoseRealtime(opts, meta, inventory, nil)
	if result.Realtime.Enabled || !hasDoctorReason(result, "frozen-lock-missing") {
		t.Fatalf("missing frozen lock: %#v", result)
	}
	plan := doctorTestPlan(t, resources.ResolveRequest{Baseline: "image"}, inventory.Baselines[1])
	if err := cliresources.SavePlan(root, "", "target:paper", plan); err != nil {
		t.Fatal(err)
	}
	result = diagnoseRealtime(opts, meta, inventory, nil)
	if !result.Realtime.Enabled || result.Realtime.Baseline.ID != "image" {
		t.Fatalf("matching lock did not pin image baseline: %#v", result)
	}
	opts.requires.Tools = []string{"biber"}
	result = diagnoseRealtime(opts, meta, inventory, nil)
	if result.Realtime.Enabled || !hasDoctorReason(result, "frozen-lock-stale") {
		t.Fatalf("stale frozen lock: %#v", result)
	}
	opts.requires.Tools = nil
	inventory.Baselines[1].Digest = strings.Repeat("e", 64)
	result = diagnoseRealtime(opts, meta, inventory, nil)
	if result.Realtime.Enabled || !hasDoctorReason(result, "locked-runtime-changed") {
		t.Fatalf("baseline drift: %#v", result)
	}
	result = diagnoseRealtime(opts, meta, inventory, errors.New("unavailable"))
	if result.Realtime.Enabled || !hasDoctorReason(result, "baseline-inventory-unavailable") {
		t.Fatalf("unavailable inventory: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, ".latexmk-cache")); !os.IsNotExist(err) {
		t.Fatalf("diagnosis created project identity: %v", err)
	}
}

func TestDoctorPathsUsesPhysicalPathsWithoutRequiringEntry(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, root, entry, want string }{
		{"root-alias", alias, filepath.Join(root, "main.tex"), "main.tex"},
		{"entry-alias", root, filepath.Join(alias, "main.tex"), "main.tex"},
		{"missing-entry", root, filepath.Join(alias, "missing", "main.tex"), "missing/main.tex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := compileOptions{projectRoot: test.root, entry: test.entry}
			if err := doctorPaths(root, config.Resolved{}, &opts); err != nil {
				t.Fatal(err)
			}
			if opts.projectRoot != root || opts.entry != test.want {
				t.Fatalf("root=%s entry=%s", opts.projectRoot, opts.entry)
			}
		})
	}
	opts := compileOptions{entry: filepath.Join(alias, "main.tex")}
	if err := doctorPaths(
		root,
		config.Resolved{},
		&opts,
	); err != nil || opts.projectRoot != root ||
		opts.entry != "main.tex" {
		t.Fatalf("implicit root differs from compilation: %#v %v", opts, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.tex")
	if err := os.WriteFile(outside, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "outside.tex")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	opts = compileOptions{projectRoot: root, entry: link}
	if err := doctorPaths(root, config.Resolved{}, &opts); err == nil {
		t.Fatal("doctor accepted an entry symlink escaping the project")
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	opts = compileOptions{projectRoot: loop}
	if err := doctorPaths(root, config.Resolved{}, &opts); err == nil {
		t.Fatal("doctor ignored a project root resolution error")
	}
}

func TestDoctorJSONErrorsRespectParsedFormat(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name string
		args []string
		json bool
	}{
		{"unknown-target", []string{"--target", "absent", "--json"}, true},
		{"invalid-value-before-json", []string{"--server-cache", "invalid", "--json"}, true},
		{"syntax-after-json", []string{"--json", "--invalid-option"}, true},
		{"syntax-before-json", []string{"--invalid-option", "--json"}, false},
		{"json-is-value", []string{"--server", "--json"}, false},
		{"json-disabled", []string{"--json=false", "--server-cache", "invalid"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := captureCommandOutput(t, func() int {
				return runDoctor(append([]string{"--token-mode", "none"}, test.args...))
			})
			if !test.json {
				if code != 2 || stderr == "" || strings.Contains(stdout, `"schemaVersion"`) {
					t.Fatalf("human error: code=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				return
			}
			var envelope agentJSONEnvelope
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("JSON error missing: code=%d stdout=%q stderr=%q: %v", code, stdout, stderr, err)
			}
			if code != 2 || stderr != "" || envelope.OK || envelope.Command != "doctor" ||
				envelope.Error == nil || envelope.Error.Code != "invalid_arguments" {
				t.Fatalf("invalid JSON error: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestDoctorChecksMetadataEngineWhenNoResourceBaseline(t *testing.T) {
	meta := doctorTestMetadata()
	meta.Capabilities.ResourceEnvironments = false
	meta.Capabilities.IsolatedWorkspaces = true
	meta.Capabilities.Engines = []string{"pdflatex"}
	opts := compileOptions{projectRoot: t.TempDir(), entry: "main.tex", engine: "xelatex", realtime: true,
		auxiliary: protocol.AuxiliaryOptions{Server: "reuse"}}
	result := diagnoseRealtime(opts, meta, client.EnvironmentInventory{}, nil)
	if result.Realtime.Enabled || !hasDoctorReason(result, "engine-unavailable") {
		t.Fatalf("unsupported legacy engine reported enabled: %#v", result)
	}
	opts.engine = "pdflatex"
	result = diagnoseRealtime(opts, meta, client.EnvironmentInventory{}, nil)
	if !result.Realtime.Enabled {
		t.Fatalf("supported metadata engine rejected: %#v", result)
	}
}

func TestDoctorJSONReportsConfigurationAndConnectionFailures(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LATEXMK_TOKEN", "")
	t.Setenv("LATEXMK_TOKEN_FILE", "")
	t.Chdir(t.TempDir())
	for _, test := range []struct {
		name, configuration, endpoint, wantCode string
		status, wantExit                        int
	}{
		{name: "configuration", configuration: `{"timeout":"bad"}`, wantCode: "invalid_arguments", wantExit: 2},
		{name: "authentication", configuration: `{"token":{"file":"absent-token"}}`, wantCode: "invalid_arguments", wantExit: 2},
		{name: "health", endpoint: "/healthz", status: http.StatusUnauthorized, wantCode: "authentication_failed", wantExit: 1},
		{name: "metadata", endpoint: "/v1/meta", status: http.StatusServiceUnavailable, wantCode: "server_error", wantExit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(".latexmk.json", []byte(test.configuration), 0600); err != nil {
				t.Fatal(err)
			}
			if test.configuration == "" {
				if err := os.Remove(".latexmk.json"); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method: %s", r.Method)
				}
				if r.URL.Path == test.endpoint {
					http.Error(w, "service unavailable", test.status)
				}
			}))
			defer server.Close()
			code, stdout, stderr := captureCommandOutput(t, func() int {
				return runDoctor([]string{"--server", server.URL, "--json"})
			})
			var envelope agentJSONEnvelope
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("invalid JSON failure: code=%d stdout=%q stderr=%q: %v", code, stdout, stderr, err)
			}
			if code != test.wantExit || stderr != "" || envelope.OK || envelope.Command != "doctor" ||
				envelope.Error == nil || envelope.Error.Code != test.wantCode {
				t.Fatalf("wrong failure boundary: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}
