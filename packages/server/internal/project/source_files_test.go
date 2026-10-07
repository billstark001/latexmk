package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func TestSnapshotTransportReadsVerifiedBlobsWithoutMaterialization(t *testing.T) {
	m, err := New(config.Config{
		StateDir: t.TempDir(), MaxFiles: 4, MaxExpandedBytes: 64, MaxStateBytes: 512,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("immutable source")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	plan, err := m.Plan("owner", protocol.UploadPlanRequest{
		ProjectID: "paper", Request: protocol.CompileRequest{Entry: "main.tex"},
		Files: []protocol.ProjectFile{{Path: "main.tex", SHA256: sha, Size: int64(len(content))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutBlob("owner", plan.UploadID, sha, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := m.Commit(context.Background(), "owner", plan.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PinSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	defer m.ReleaseSnapshot(snapshot.ID)
	files, err := m.SourceFiles(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	file := files["main.tex"]
	read := func() ([]byte, error) {
		input, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = input.Close() }()
		var out bytes.Buffer
		err = safefs.CopyVerified(&out, input, file.Size, file.SHA256)
		return out.Bytes(), err
	}
	got, err := read()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("source transport = %q, %v", got, err)
	}
	blob := m.blobPath("owner", sha)
	if err := os.WriteFile(blob, bytes.Repeat([]byte("x"), len(content)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); err == nil {
		t.Fatal("corrupt source blob escaped transport verification")
	}
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), blob); err != nil {
		t.Fatal(err)
	}
	if input, err := file.Open(); err == nil {
		_ = input.Close()
		t.Fatal("symlinked source blob was opened")
	}
	for _, bad := range [][]protocol.ProjectFile{
		{{Path: "../outside", SHA256: sha, Size: 1}},
		{{Path: "main.tex", SHA256: "bad", Size: 1}},
		{{Path: "main.tex", SHA256: sha, Size: -1}},
		{{Path: "main.tex", SHA256: sha, Size: 65}},
		{snapshot.Files[0], snapshot.Files[0]},
	} {
		snapshot.Files = bad
		if _, err := m.SourceFiles(snapshot); err == nil {
			t.Fatalf("invalid source manifest accepted: %+v", bad)
		}
	}
}

func TestSnapshotTransportRejectsMissingBlobAtReadTime(t *testing.T) {
	m, err := New(config.Config{StateDir: t.TempDir(), MaxFiles: 4, MaxExpandedBytes: 64, MaxStateBytes: 512}, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := protocol.ProjectFile{Path: "main.tex", SHA256: hex.EncodeToString(make([]byte, 32)), Size: 0}
	files, err := m.SourceFiles(Snapshot{OwnerID: "owner", Files: []protocol.ProjectFile{file}})
	if err != nil {
		t.Fatal(err)
	}
	if reader, err := files[file.Path].Open(); err == nil {
		_, _ = io.Copy(io.Discard, reader)
		_ = reader.Close()
		t.Fatal("missing blob was readable")
	}
}
