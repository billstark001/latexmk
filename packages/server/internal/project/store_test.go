package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/config"
)

func TestPlanOnlyRequestsMissingContentAndMaterializesSnapshot(t *testing.T) {
	state := t.TempDir()
	m, err := New(config.Config{StateDir: state, MaxFiles: 10, MaxExpandedBytes: 1024, MaxStateBytes: 1024}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("\\documentclass{article}")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	request := api.UploadPlanRequest{
		ProjectID: "paper",
		Request:   api.CompileRequest{ProtocolVersion: api.ProtocolVersion, Entry: "main.tex"},
		Files:     []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}},
	}
	plan, err := m.Plan("member", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Missing) != 1 || plan.Missing[0] != sha {
		t.Fatalf("unexpected first plan: %#v", plan)
	}
	if err := m.PutBlob("member", plan.UploadID, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := m.Commit(context.Background(), "member", plan.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.Materialize(snapshot, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "main.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("materialized content = %q", got)
	}
	second, err := m.Plan("member", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Missing) != 0 {
		t.Fatalf("expected reuse, got missing %v", second.Missing)
	}
}

func TestSnapshotIDUsesCanonicalManifestOrder(t *testing.T) {
	a := api.ProjectFile{Path: "a.tex", SHA256: strings.Repeat("a", 64), Size: 1}
	b := api.ProjectFile{Path: "b.tex", SHA256: strings.Repeat("b", 64), Size: 2}
	first, err := NewSnapshot("member", "paper", []api.ProjectFile{b, a})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSnapshot("member", "paper", []api.ProjectFile{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("snapshot IDs differ by input order: %q != %q", first.ID, second.ID)
	}
	if first.Files[0].Path != "a.tex" {
		t.Fatalf("snapshot files are not canonical: %#v", first.Files)
	}
}

func TestPutBlobRejectsExtraData(t *testing.T) {
	m, err := New(config.Config{StateDir: t.TempDir(), MaxFiles: 10, MaxExpandedBytes: 1024, MaxStateBytes: 1024}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("a")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	plan, err := m.Plan("member", api.UploadPlanRequest{ProjectID: "paper", Files: []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutBlob("member", plan.UploadID, sha, bytes.NewReader([]byte("ab"))); err == nil {
		t.Fatal("expected extra upload data to fail")
	}
}

func TestPutBlobEnforcesStateStorageLimit(t *testing.T) {
	m, err := New(config.Config{StateDir: t.TempDir(), MaxFiles: 10, MaxExpandedBytes: 1024, MaxStateBytes: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("ab")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	plan, err := m.Plan("member", api.UploadPlanRequest{ProjectID: "paper", Files: []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutBlob("member", plan.UploadID, sha, bytes.NewReader(content)); err == nil {
		t.Fatal("expected state storage limit to reject blob")
	}
}

func TestPlanEnforcesBlobAndSessionLimits(t *testing.T) {
	m, err := New(config.Config{StateDir: t.TempDir(), MaxFiles: 10, MaxUploadBytes: 1, MaxExpandedBytes: 1024, MaxStateBytes: 1024, MaxUploadSessions: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("ab")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	request := api.UploadPlanRequest{ProjectID: "paper", Files: []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}}}
	if _, err := m.Plan("member", request); err == nil {
		t.Fatal("expected per-blob upload limit to reject manifest")
	}
	m.cfg.MaxUploadBytes = 1024
	if _, err := m.Plan("member", request); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Plan("member", request); err == nil {
		t.Fatal("expected upload session limit to reject a second plan")
	}
}

func TestPruneKeepsReferencedBlobsAndRemovesExpiredCache(t *testing.T) {
	cfg := config.Config{
		StateDir:           t.TempDir(),
		MaxFiles:           10,
		MaxUploadBytes:     1024,
		MaxExpandedBytes:   1024,
		MaxStateBytes:      4096,
		ResultRetention:    time.Hour,
		SnapshotRetention:  time.Hour,
		BlobRetention:      time.Hour,
		StateSweepInterval: time.Hour,
	}
	m, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("source")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	request := api.UploadPlanRequest{ProjectID: "paper", Files: []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}}}
	plan, err := m.Plan("member", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutBlob("member", plan.UploadID, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Commit(context.Background(), "member", plan.UploadID); err != nil {
		t.Fatal(err)
	}
	orphan := m.blobPath("member", strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := m.ResultPath("member", "job_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{m.blobPath("member", sha), orphan, result} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.blobPath("member", sha)); err != nil {
		t.Fatalf("referenced blob was removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan blob still exists: %v", err)
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatalf("expired result still exists: %v", err)
	}
}

func TestPruneKeepsBlobPinnedByOlderQueuedSnapshot(t *testing.T) {
	cfg := config.Config{
		StateDir: t.TempDir(), MaxFiles: 10, MaxUploadBytes: 1024,
		MaxExpandedBytes: 1024, MaxStateBytes: 4096,
		SnapshotRetention: time.Hour, BlobRetention: time.Hour,
	}
	m, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(content string) Snapshot {
		t.Helper()
		digest := sha256.Sum256([]byte(content))
		sha := hex.EncodeToString(digest[:])
		plan, err := m.Plan("member", api.UploadPlanRequest{ProjectID: "paper", Files: []api.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Missing) > 0 {
			if err := m.PutBlob("member", plan.UploadID, sha, strings.NewReader(content)); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, _, err := m.Commit(context.Background(), "member", plan.UploadID)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}

	first := commit("first")
	if err := m.PinSnapshot(first); err != nil {
		t.Fatal(err)
	}
	_ = commit("second")
	firstBlob := m.blobPath("member", first.Files[0].SHA256)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(firstBlob, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstBlob); err != nil {
		t.Fatalf("queued snapshot blob was removed: %v", err)
	}

	m.ReleaseSnapshot(first.ID)
	if _, err := m.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstBlob); !os.IsNotExist(err) {
		t.Fatalf("released old snapshot blob still exists: %v", err)
	}
}
