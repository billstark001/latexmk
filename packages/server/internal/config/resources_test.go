package config

import (
	"os"
	"path/filepath"
	"testing"
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
