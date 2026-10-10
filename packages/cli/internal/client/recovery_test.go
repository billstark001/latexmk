package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestDetachedUploadUsesTheCapturedManifestBytes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.tex")
	if err := os.WriteFile(source, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("before"))
	expected := hex.EncodeToString(digest[:])
	var uploaded string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(
				w,
			).Encode(
				protocol.Metadata{
					ProtocolVersion: protocol.Version,
					Capabilities:    protocol.Capabilities{IncrementalUpload: true, QueuedJobs: true},
				},
			)
		case r.URL.Path == "/v1/uploads/plans":
			var plan protocol.UploadPlanRequest
			if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
				t.Error(err)
			}
			if len(plan.Files) != 1 || plan.Files[0].SHA256 != expected {
				t.Errorf("unexpected captured manifest: %+v", plan.Files)
			}
			if err := os.WriteFile(source, []byte("after!"), 0600); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_test", Missing: []string{expected}})
		case strings.Contains(r.URL.Path, "/blobs/"):
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			uploaded = string(raw)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/uploads/upl_test/commit":
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_test", Status: "queued"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.ProjectID, c.UploadMode = root, "paper", "all"
	if _, err := c.StartCompile(
		context.Background(),
		protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"},
	); err != nil {
		t.Fatal(err)
	}
	if uploaded != "before" {
		t.Fatalf("uploaded editor bytes diverged from captured hash: %q", uploaded)
	}
}

func TestMissingFileRecoverySharesRoundAndCapturedByteLimits(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"main.tex", "one.tex", "two.tex", "three.tex", "four.tex"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := &Client{ProjectRoot: root, UploadMode: "all"}
	recovery := MissingFileRecovery{}
	for i, name := range []string{"one.tex", "two.tex", "three.tex"} {
		paths, err := c.ResolveMissingFiles([]string{name, name}, nil, &recovery)
		if err != nil || len(paths) != 1 || recovery.Rounds != i+1 {
			t.Fatalf("bounded recovery = %v, %+v, %v", paths, recovery, err)
		}
	}
	if _, err := c.ResolveMissingFiles([]string{"four.tex"}, nil, &recovery); err == nil {
		t.Fatal("fourth recovery round accepted")
	}
	if err := recovery.ValidateCaptured(
		[]projectarchive.File{{Path: "one.tex", Size: maxNeedsFileBytes + 1}},
	); err == nil {
		t.Fatal("growth after stat bypassed captured-byte limit")
	}
	recovery.Rounds = 0
	if _, err := c.ResolveMissingFiles([]string{"one.tex"}, nil, &recovery); err == nil {
		t.Fatal("duplicate consumed another recovery round")
	}
	if recovery.Rounds != 0 {
		t.Fatal("refused recovery mutated its round budget")
	}
}

func TestMissingFileRecoveryRejectsInvalidBudgetBeforeMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "new.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &Client{ProjectRoot: root, UploadMode: "all"}
	for _, bytes := range []int64{-1, math.MaxInt64, maxNeedsFileBytes} {
		recovery := MissingFileRecovery{addedBytes: bytes}
		if _, err := c.ResolveMissingFiles([]string{"new.tex"}, nil, &recovery); err == nil {
			t.Errorf("accepted byte budget %d", bytes)
		}
		if recovery.Rounds != 0 || len(recovery.Additional) != 0 || recovery.addedBytes != bytes {
			t.Fatal("rejected recovery changed state")
		}
	}
	if _, err := c.ResolveMissingFiles([]string{"new.tex"}, nil, nil); err == nil {
		t.Fatal("accepted nil recovery")
	}
}

func TestMissingFileRecoveryKeepsGrowthBudgetAfterShrink(t *testing.T) {
	root := t.TempDir()
	for name, size := range map[string]int64{"prior.tex": 40 << 20, "new.tex": 1, "later.tex": 30 << 20} {
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(size); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	c := &Client{ProjectRoot: root, UploadMode: "all"}
	recovery := MissingFileRecovery{Additional: []string{"prior.tex"}, addedBytes: 1, Rounds: 1}
	if _, err := c.ResolveMissingFiles([]string{"new.tex"}, nil, &recovery); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(root, "prior.tex"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveMissingFiles([]string{"later.tex"}, nil, &recovery); err == nil {
		t.Fatal("shrinking an earlier file reset its historical growth budget")
	}
	if recovery.Rounds != 2 || len(recovery.Additional) != 2 {
		t.Fatal("refused recovery changed state")
	}
}

func TestMissingFileRecoveryRemembersCapturedGrowth(t *testing.T) {
	recovery := MissingFileRecovery{Additional: []string{"prior.tex"}, addedBytes: 1}
	if err := recovery.ValidateCaptured([]projectarchive.File{{Path: "prior.tex", Size: 40 << 20}}); err != nil {
		t.Fatal(err)
	}
	if recovery.addedBytes != 40<<20 {
		t.Fatal("capture growth did not advance the historical budget")
	}
	if err := recovery.ValidateCaptured(
		[]projectarchive.File{{Path: "prior.tex", Size: maxNeedsFileBytes + 1}},
	); err == nil {
		t.Fatal("accepted an oversized capture")
	}
	if recovery.addedBytes != 40<<20 {
		t.Fatal("rejected capture changed the historical budget")
	}
}
