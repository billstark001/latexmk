package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
)

func TestResultPublicationPreservesOldGenerationOnFailure(t *testing.T) {
	m, _, _ := cacheFixture(t)
	original := cacheOutput(t, t.TempDir(), map[string]string{"main.pdf": "original pdf"})
	path, err := m.WriteResult("alice", "job1", original)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	used := m.stateBytes
	changed := cacheOutput(t, t.TempDir(), map[string]string{"main.pdf": "different pdf"})
	artifact := changed.Files[0]
	if err := os.WriteFile(
		filepath.Join(artifact.Workspace, artifact.RelativePath),
		[]byte("tampered data"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteResult("alice", "job1", changed); err == nil {
		t.Fatal("accepted changed artifact")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || m.stateBytes != used {
		t.Fatalf("failed publication changed old result/accounting: %v", err)
	}
	m.cfg.MaxStateBytes = used + 1
	if _, err := m.WriteResult("alice", "job1", original); err == nil {
		t.Fatal("quota did not include staged bytes")
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || m.stateBytes != used {
		t.Fatalf("quota failure changed old result/accounting: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staged files leaked: %v %v", entries, err)
	}
	m.cfg.MaxStateBytes = 1 << 20
	if _, err := m.WriteResult("alice", "job1", compile.Output{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || m.stateBytes != info.Size() {
		t.Fatalf("replacement accounting: %v", err)
	}
}

func TestStagedStateCopyLeavesStorageResponsiveAndKeepsExactQuota(t *testing.T) {
	m, _, _ := cacheFixture(t)
	path := m.blobPath("alice", strings.Repeat("a", 64))
	ready, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	staged := make(chan *StatePublication, 1)
	errs := make(chan error, 1)
	go func() {
		p, err := m.stageState(path, 8, func(writer io.Writer) error {
			if _, err := writer.Write([]byte("part")); err != nil {
				return err
			}
			close(ready)
			<-release
			_, err := writer.Write([]byte("data"))
			return err
		})
		staged <- p
		errs <- err
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("stage did not start")
	}
	done := make(chan error, 1)
	go func() {
		_, err := m.CollectUnreferencedBlobs(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("collector rejected an active staged sibling: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("copy held the storage mutex")
	}
	m.mu.Lock()
	used, pending := m.stateBytes, m.pendingBytes
	m.mu.Unlock()
	if used != 0 || pending != 8 {
		t.Fatalf("sweep double-counted staging: stored=%d pending=%d", used, pending)
	}
	release <- struct{}{}
	p := <-staged
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	if m.stateBytes != 8 || m.pendingBytes != 0 {
		t.Fatalf("publication accounting: stored=%d pending=%d", m.stateBytes, m.pendingBytes)
	}
	if err := p.Commit(); err == nil {
		t.Fatal("published a closed stage twice")
	}
}

func TestFailedStateStageReleasesOriginalReservation(t *testing.T) {
	m, _, _ := cacheFixture(t)
	path, err := m.LiveCachePath("alice", "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		_, err := m.stageState(path, 8, func(writer io.Writer) error {
			if _, err := writer.Write([]byte("short")); err != nil {
				return err
			}
			if fail {
				return errors.New("copy interrupted")
			}
			return nil
		})
		if err == nil || m.pendingBytes != 0 || m.stateBytes != 0 {
			t.Fatalf("failed stage leaked its reservation: %v, %d, %d", err, m.pendingBytes, m.stateBytes)
		}
	}
}

func TestStartupRemovesOrphanedStagedSiblings(t *testing.T) {
	root := t.TempDir()
	blobs := filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(blobs, ".latexmk-"+strings.Repeat("a", 32))
	if err := os.WriteFile(orphan, []byte("interrupted publication"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(config.Config{StateDir: root, MaxStateBytes: 1}, nil)
	if err != nil || m.stateBytes != 0 {
		t.Fatalf("orphan consumed startup quota: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan was retained: %v", err)
	}
}
