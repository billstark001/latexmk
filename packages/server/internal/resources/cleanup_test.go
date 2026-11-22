package resources

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestReturnedInventoriesAndLeasesOwnTheirNestedData(t *testing.T) {
	m := testManager(t)
	baseline := m.baselines[m.defaultID]
	baseline.Toolchain = map[string]string{"xelatex": "runtime version"}
	baseline.Fonts = []model.Font{{File: "Paper.ttf", Family: "Paper"}}
	baseline.Packages = []model.Package{{Name: "algorithms", Revision: 1}}
	var err error
	baseline.Digest, err = model.BaselineDigest(baseline)
	if err != nil {
		t.Fatal(err)
	}
	m.baselines[m.defaultID] = baseline
	request := resourceRequest("snapshot", "data")
	request.Resources[0].LicenseFiles = []string{"asset.dat"}
	request.Resources = append(
		request.Resources,
		model.Resource{ID: "system", Type: "texlive", Packages: []string{"algorithms"}},
	)
	request.Provided = []model.Resource{{ID: "provided", Type: "texlive", Packages: []string{"algorithms"}}}
	request.Requires = model.Requirements{Tools: []string{"xelatex"}, Fonts: []model.FontRequirement{{Family: "Paper"}}}
	env := uploadResource(t, m, "alice", request, "snapshot")
	lease, err := m.Pin("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	want, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	mutateBaseline := func(value model.Baseline) {
		value.Engines[0] = "changed"
		value.Toolchain["xelatex"] = "changed"
		value.Fonts[0].Family = "changed"
		value.Packages[0].Name = "changed"
	}
	mutateEnvironment := func(value model.Environment) {
		mutateBaseline(value.Baseline)
		for i := range value.Resources {
			resource := &value.Resources[i]
			if len(resource.Files) > 0 {
				resource.Files[0].Path = "changed"
				resource.LicenseFiles[0] = "changed"
			}
			if len(resource.Packages) > 0 {
				resource.Packages[0] = "changed"
			}
		}
		value.Provided[0].Packages[0] = "changed"
		value.Requires.Fonts[0].Family = "changed"
		value.Requires.Tools[0] = "changed"
	}
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			for range 20 {
				listed := m.List()
				mutateBaseline(listed[0])
				current, err := m.Inspect("alice", env.Digest)
				if err != nil {
					failures <- err
					return
				}
				mutateEnvironment(current)
				mutateEnvironment(lease.Environment())
				files := lease.Files()
				files[0].Path = "changed"
			}
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	current, err := m.Inspect("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("mutating returned nested data changed the manager's environment")
	}
	if lease.Files()[0].Path != "vendor/asset.dat" {
		t.Fatal("mutating a returned descriptor changed a pinned lease")
	}
	verified, err := m.Pin("alice", env.Digest)
	if err != nil {
		t.Fatalf("returned inventory mutation corrupted runtime identity: %v", err)
	}
	verified.Release()
}

func blockRecordWrite(t *testing.T, m *Manager, digest string) string {
	t.Helper()
	path := filepath.Join(m.dir, "records", digest+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPreparePersistenceFailureLeavesNoPublishedTreeAndCanRetry(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("macro", "texmf")
	request.Resources[0].Files[0].Path = "tex/latex/paper/paper.sty"
	request.Resources[0].Mount = "."
	env, err := m.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.PutBlob(
		context.Background(),
		"alice",
		env.Digest,
		request.Resources[0].Files[0].SHA256,
		strings.NewReader("macro"),
	); err != nil {
		t.Fatal(err)
	}
	blocked := blockRecordWrite(t, m, env.Digest)
	if _, err = m.Prepare(context.Background(), "alice", env.Digest); err == nil {
		t.Fatal("record publication failure was ignored")
	}
	rec := m.records[env.Digest]
	if rec.Prepared || rec.refs != 0 || rec.busy != nil || len(rec.TreeFiles) != 0 {
		t.Fatalf("failed preparation retained committed state: %+v", rec)
	}
	if _, err = os.Stat(m.treePath(env.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preparation published a tree: %v", err)
	}
	staging, err := os.ReadDir(filepath.Join(m.dir, "staging"))
	if err != nil || len(staging) != 0 {
		t.Fatalf("failed preparation retained staging files: %v %v", staging, err)
	}
	if err = os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	env, err = m.Prepare(context.Background(), "alice", env.Digest)
	if err != nil || !env.Ready {
		t.Fatalf("failed preparation could not be retried: %+v %v", env, err)
	}
	restarted, err := New(m.cfg, protocol.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := restarted.Pin("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestSweepPersistenceFailureRetainsAuthorizationUntilRetry(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("data", "data")
	env := uploadResource(t, m, "alice", request, "data")
	if _, err := m.Resolve(context.Background(), "bob", request); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := m.records[env.Digest]
	rec.Owners[ownerKey("alice")].Used = now.Add(-m.cfg.ResourceRetention - time.Hour)
	blocked := blockRecordWrite(t, m, env.Digest)
	if err := m.Sweep(now); err == nil {
		t.Fatal("failed authorization persistence was ignored")
	}
	if rec.Owners[ownerKey("alice")] == nil {
		t.Fatal("failed sweep changed only in-memory authorization")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := m.Sweep(now); err != nil {
		t.Fatal(err)
	}
	if rec.Owners[ownerKey("alice")] != nil || rec.Owners[ownerKey("bob")] == nil {
		t.Fatal("sweep retry did not commit the intended owner set")
	}
	if _, err := New(m.cfg, protocol.Metadata{}); err != nil {
		t.Fatalf("sweep retry wrote an invalid durable record: %v", err)
	}
}

func TestVerificationFailuresReleaseTemporaryReferences(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("original", "data")
	env := uploadResource(t, m, "alice", request, "original")
	path := m.blobPath(request.Resources[0].Files[0].SHA256)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pin("alice", env.Digest); err == nil {
		t.Fatal("corrupt input was pinned")
	}
	if _, _, err := m.OpenExport(
		"alice",
		env.Digest,
		"paper-assets",
		request.Resources[0].Files[0].SHA256,
	); err == nil {
		t.Fatal("corrupt input was exported")
	}
	if m.records[env.Digest].refs != 0 {
		t.Fatal("failed verification retained a garbage-collection lease")
	}
}

func TestCopiedPossessionHonorsFalseProofAndCanceledPrepare(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("data", "data")
	env, err := m.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	sha := request.Resources[0].Files[0].SHA256
	m.records[env.Digest].Owners[ownerKey("alice")].Uploaded[sha] = false
	request.Resources[0].Mount = "other"
	other, err := m.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Missing) != 1 {
		t.Fatal("false possession proof was promoted while copying authorization")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.Prepare(ctx, "alice", env.Digest); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preparation changed state: %v", err)
	}
	if m.records[env.Digest].Prepared {
		t.Fatal("canceled preparation was published")
	}
}

func TestNullPersistedOwnerIsReportedAsCorruption(t *testing.T) {
	m := testManager(t)
	env, err := m.Resolve(context.Background(), "alice", resourceRequest("data", "data"))
	if err != nil {
		t.Fatal(err)
	}
	rec := m.records[env.Digest]
	rec.Owners[ownerKey("alice")] = nil
	if err = m.saveLocked(rec); err != nil {
		t.Fatal(err)
	}
	if _, err = New(
		m.cfg,
		protocol.Metadata{},
	); err == nil ||
		!strings.Contains(err.Error(), "corrupt environment authorization") {
		t.Fatalf("null persisted owner was not rejected: %v", err)
	}
}
