package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedresources "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestResourceDirectoryRejectsStateOverlapAndSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []string{state, root, filepath.Join(state, "blobs", "resources"), filepath.Join(state, "live-cache", "assets")} {
		if path, err := (Config{StateDir: state, ResourceCacheDir: cache}).ResourceDirectory(); err == nil {
			t.Fatalf("accepted overlapping resource cache %s", path)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(state, "blobs"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := (Config{StateDir: state, ResourceCacheDir: filepath.Join(alias, "new")}).ResourceDirectory(); err == nil {
		t.Fatal("symlink alias bypassed reserved directory boundary")
	}
	canonicalState, err := filepath.EvalSymlinks(state)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalState, "environments")
	got, err := (Config{StateDir: state}).ResourceDirectory()
	if err != nil || got != want {
		t.Fatalf("default directory: %s %v", got, err)
	}
	outside := filepath.Join(root, "resource-cache")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = (Config{StateDir: state, ResourceCacheDir: outside}).ResourceDirectory(); err != nil ||
		got != filepath.Join(canonicalRoot, "resource-cache") {
		t.Fatalf("independent directory: %s %v", got, err)
	}
}

func TestResourceSourcesInheritRuntimeIndexAndExplicitAssertionWins(t *testing.T) {
	inventory := t.TempDir()
	t.Setenv("LATEXMK_RUNTIME_INVENTORY_DIR", inventory)
	index := sharedresources.PinnedPackageIndex{Version: 1, Path: "runtime.tlpdb", SHA256: strings.Repeat("a", 64),
		Repository: "https://snapshot.example/2026/09/09/tlnet", TexliveYear: 2026}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inventory, "texlive-package-index.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	policyDir := t.TempDir()
	policyPath := filepath.Join(policyDir, "sources.json")
	policy := sharedresources.SourcePolicy{Version: 1, ArchiveMirrors: []string{"https://mirror.example/tlnet-archive"}}
	writePolicy := func() {
		t.Helper()
		data, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(policyPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writePolicy()
	t.Setenv("LATEXMK_TEXLIVE_SOURCES_FILE", policyPath)
	var cfg Config
	if err := loadResourceConfig(&cfg); err != nil || cfg.ResourcePackageIndex == nil ||
		cfg.ResourcePackageIndex.Path != filepath.Join(inventory, "runtime.tlpdb") {
		t.Fatalf("runtime assertion not inherited: %#v %v", cfg.ResourcePackageIndex, err)
	}
	index.Path = "explicit.tlpdb"
	policy.PackageIndex = &index
	writePolicy()
	cfg = Config{}
	if err := loadResourceConfig(
		&cfg,
	); err != nil ||
		cfg.ResourcePackageIndex.Path != filepath.Join(policyDir, "explicit.tlpdb") {
		t.Fatalf("explicit assertion did not override runtime: %#v %v", cfg.ResourcePackageIndex, err)
	}
	t.Setenv("LATEXMK_TEXLIVE_SOURCES_FILE", "")
	cfg = Config{}
	if err := loadResourceConfig(
		&cfg,
	); err != nil || cfg.ResourceSourcePolicy != nil ||
		cfg.ResourcePackageIndex == nil {
		t.Fatalf("absent transport policy did not preserve nil semantics: %#v %v", cfg, err)
	}
}

func TestResourceConfigurationRejectsDisabledOrOverflowingLimits(t *testing.T) {
	t.Setenv("LATEXMK_AUTH_MODE", "none")
	t.Setenv("LATEXMK_MAX_CONCURRENT_RESOURCE_PREPARES", "0")
	if _, err := Load(); err == nil {
		t.Fatal("zero prepare semaphore would deadlock all preparations")
	}
	t.Setenv("LATEXMK_MAX_CONCURRENT_RESOURCE_PREPARES", "2")
	t.Setenv("LATEXMK_MAX_RESOURCE_FILES", "1000001")
	if _, err := Load(); err == nil {
		t.Fatal("accepted an overflowing resource metadata budget")
	}
}
