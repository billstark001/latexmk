package project

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
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
