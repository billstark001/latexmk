package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func TestConfigControlPathsIncludeConcreteCandidatesOnce(t *testing.T) {
	root := t.TempDir()
	paths := configControlPaths(root, config.Resolved{
		ConfigPath: filepath.Join(root, config.FileName),
		DenyFiles:  []string{"", "credentials/token", "", "credentials/token"},
	})
	want := map[string]bool{
		filepath.Join(root, config.FileName):     true,
		filepath.Join(root, config.EnvFileName):  true,
		filepath.Join(root, "credentials/token"): true,
	}
	for _, path := range paths {
		if !want[path] {
			t.Fatalf("unexpected or duplicate control path %q: %v", path, paths)
		}
		delete(want, path)
	}
	if len(want) != 0 {
		t.Fatal("missing configuration or credential candidates", want)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New("http://127.0.0.1:8080", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.UploadMode = root, "all"
	opts := compileOptions{
		projectRoot: root, controlFiles: paths,
		watchInterval: 20 * time.Millisecond, watchDebounce: 10 * time.Millisecond, watchMaxWait: time.Second,
	}
	tracker, err := newSourceTracker(c, protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"}, opts,
		[]projectarchive.File{{Path: "main.tex", Source: filepath.Join(root, "main.tex")}})
	if err != nil {
		t.Fatal("ordinary tracker rejected optional configuration candidates", err)
	}
	check := func() {
		t.Helper()
		seen := make(map[string]bool)
		for _, target := range tracker.targets {
			if target.Name == "" || target.Path == "" || seen[target.Path] {
				t.Fatalf("control targets were duplicated or left incomplete: %+v", tracker.targets)
			}
			seen[target.Path] = true
		}
	}
	check()
	if _, err := tracker.Refresh(); err != nil {
		t.Fatal(err)
	}
	check()
	opts.controlFiles = []string{""}
	if _, err := newSourceTracker(c, protocol.CompileRequest{}, opts, nil); err == nil {
		t.Fatal("invalid internal control target was silently discarded")
	}
}

func TestRealtimeResourceMemberCreationInvalidatesSettings(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "managed", "nested")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New("http://127.0.0.1:8080", "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.UploadMode, c.IncludeFiles = root, "manifest", []string{"main.tex"}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	observation := observeLive(ctx, c, protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"},
		compileOptions{
			projectRoot: root, resourceIDs: []string{"body"},
			resourceSpecs: map[string]resources.Spec{"body": {Type: "files", Path: "managed", Mount: "vendor"}},
			watchInterval: 20 * time.Millisecond, watchDebounce: 10 * time.Millisecond, watchMaxWait: time.Second,
		}, cancel, nil)
	if err := os.WriteFile(filepath.Join(directory, "added.sty"), []byte("new resource"), 0600); err != nil {
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
		t.Fatal("resource addition was treated as an ordinary source change")
	}
}
