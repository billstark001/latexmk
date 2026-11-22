package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/argv"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestRemoteCleanupPlanIsPrivateAndContainsNoToken(t *testing.T) {
	isolateCleanupCache(t)
	report := protocol.CleanupReport{
		ProjectID:  "project-test",
		Scope:      "results",
		DryRun:     true,
		PlanDigest: strings.Repeat("a", 64),
	}
	plan, err := createRemoteCleanupPlan("https://latex.example.edu", report.ProjectID, report.Scope, report)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := cleanupPlansDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, plan.ID+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("plan permissions = %o, want private", info.Mode().Perm())
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(payload)), "token") {
		t.Fatalf("plan unexpectedly contains token material: %s", payload)
	}
	loaded, loadedPath, err := loadRemoteCleanupPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PlanDigest != report.PlanDigest || loadedPath != path {
		t.Fatalf("loaded plan mismatch: %#v, %s", loaded, loadedPath)
	}
	if err := consumeRemoteCleanupPlan(path); err != nil {
		t.Fatal(err)
	}
	if err := consumeRemoteCleanupPlan(path); err == nil {
		t.Fatal("expected a consumed plan to be one-time")
	}
}

func TestParseRemoteCleanupRequiresPlanForApply(t *testing.T) {
	for _, test := range []struct {
		args    []string
		invalid bool
	}{{args: []string{"--scope", "project", "--yes"}, invalid: true}, {args: []string{"--scope", "project"}}} {
		changes := remoteCleanArguments()
		parsed, err := argv.Parse("remote-clean", test.args, changes.Flags()...)
		if err != nil {
			t.Fatal(err)
		}
		var options remoteCleanOptions
		if err := changes.Apply(&options); err != nil {
			t.Fatal(err)
		}
		if parsed.Args().Len() != 0 {
			t.Fatal("unexpected positional arguments")
		}
		if err := validateRemoteCleanOptions(options); (err != nil) != test.invalid {
			t.Fatalf("validation %q = %v", test.args, err)
		}
	}
}

func TestCleanupPlanRejectsSymlinkedDirectoryOnLoad(t *testing.T) {
	isolateCleanupCache(t)
	report := protocol.CleanupReport{
		ProjectID:  "paper",
		Scope:      "results",
		DryRun:     true,
		PlanDigest: strings.Repeat("a", 64),
	}
	plan, err := createRemoteCleanupPlan("https://latex.example.edu", "paper", "results", report)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := cleanupPlansDir()
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "plans")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := loadRemoteCleanupPlan(plan.ID); err == nil {
		t.Fatal("loaded a plan through a symlinked directory")
	}
	if err := saveRemoteCleanupPlan(plan); err == nil {
		t.Fatal("saved a plan through a symlinked directory")
	}
}

func TestCleanupPlanRejectsMalformedServerURL(t *testing.T) {
	isolateCleanupCache(t)
	report := protocol.CleanupReport{
		ProjectID:  "paper",
		Scope:      "results",
		DryRun:     true,
		PlanDigest: strings.Repeat("a", 64),
	}
	for _, server := range []string{"https://example.test?", "https://example.test#", "https://:443"} {
		if _, err := createRemoteCleanupPlan(server, "paper", "results", report); err == nil {
			t.Errorf("accepted server %q", server)
		}
	}
}

func isolateCleanupCache(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("LocalAppData", dir)
}
