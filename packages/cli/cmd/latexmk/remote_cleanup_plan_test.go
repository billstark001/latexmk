package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestRemoteCleanupPlanIsPrivateAndContainsNoToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	if info.Mode().Perm()&0o077 != 0 {
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
	if err := parseRemoteCleanArgs([]string{"--scope", "project", "--yes"}, &remoteCleanOptions{}); err == nil {
		t.Fatal("expected --yes without a plan to fail")
	}
	if err := parseRemoteCleanArgs([]string{"--scope", "project"}, &remoteCleanOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPlanRejectsSymlinkedDirectoryOnLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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
