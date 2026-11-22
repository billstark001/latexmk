package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/config"
)

func TestResourceCacheAccountingDoesNotConsumeSourceQuota(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[custom], func(t *testing.T) {
			state := t.TempDir()
			resourceDir := filepath.Join(state, "environments")
			cfg := config.Config{StateDir: state, MaxStateBytes: 64, MaxFiles: 10, MaxExpandedBytes: 64}
			if custom {
				resourceDir = filepath.Join(state, "custom-resources")
				cfg.ResourceCacheDir = resourceDir
			}
			if err := os.MkdirAll(resourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(resourceDir, "font-blob")
			if err := os.WriteFile(cache, make([]byte, 1024), 0600); err != nil {
				t.Fatal(err)
			}
			manager, err := New(cfg, nil)
			if err != nil || manager.stateBytes != 0 {
				t.Fatalf("resource cache charged as project state: %v", err)
			}
			if _, err := manager.Prune(context.Background()); err != nil {
				t.Fatal(err)
			}
			if manager.stateBytes != 0 {
				t.Fatal("sweep charged resource cache to source quota")
			}
			if _, err := os.Stat(cache); err != nil {
				t.Fatal("project sweep touched the independent resource cache", err)
			}
		})
	}
}
