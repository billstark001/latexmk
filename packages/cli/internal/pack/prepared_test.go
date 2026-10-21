package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedZIPMetadataPrecedesAtomicPublication(t *testing.T) {
	output := filepath.Join(t.TempDir(), "source.zip")
	if err := os.WriteFile(output, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := Prepare(ctx, output, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prepared.Close() }()
	before, err := os.ReadFile(output)
	if err != nil || string(before) != "previous" {
		t.Fatal("Prepare replaced published output", string(before), err)
	}
	cancel()
	if err := prepared.Commit(); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled ZIP was committed", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	prepared, err = Prepare(context.Background(), output, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prepared.Close() }()
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if prepared.Size != int64(len(data)) || prepared.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("pre-publication ZIP metadata does not match bytes")
	}
}
