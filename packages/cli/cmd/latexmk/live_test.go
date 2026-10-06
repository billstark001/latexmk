package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

func TestRealtimeSettingsChangeCancelsInFlightOperations(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, ".latexmk.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New("http://127.0.0.1:8080", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.UploadMode = root, "all"
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	observation := observeLive(
		ctx,
		c,
		protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"},
		compileOptions{
			projectRoot:   root,
			watchInterval: 20 * time.Millisecond,
			watchDebounce: 10 * time.Millisecond,
			controlFiles:  []string{cfg},
		},
		cancel,
	)
	if err := os.WriteFile(cfg, []byte("{\"outDir\":\"changed\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errLiveSettingsChanged) {
			t.Fatal(context.Cause(ctx))
		}
	case err := <-observation.errors:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("settings change left upload context active")
	}
}
