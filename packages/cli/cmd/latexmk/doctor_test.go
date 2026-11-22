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
