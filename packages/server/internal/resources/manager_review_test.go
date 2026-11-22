package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func newReviewManager(t *testing.T, resources []catalogResource) *Manager {
	t.Helper()
	registryFile := filepath.Join(t.TempDir(), "registry.json")
	data, err := json.Marshal(registry{Resources: resources})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(config.Config{
		StateDir: t.TempDir(), ResourceInventoryDir: t.TempDir(), ResourceRegistryFile: registryFile,
		Engines: []string{"xelatex"}, MaxResourceBytes: 4096, ResourceOwnerBytes: 8192, ResourceCacheBytes: 1 << 20,
	}, protocol.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func reviewFile(name, contents string) model.File {
	digest := sha256.Sum256([]byte(contents))
	return model.File{Path: name, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(contents))}
}

func TestServerResourceCannotLaunderRedistributionThroughFileProof(t *testing.T) {
	directory := t.TempDir()
	contents := "private institution font bytes"
	if err := os.WriteFile(filepath.Join(directory, "Institution.otf"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	manager := newReviewManager(
		t,
		[]catalogResource{
			{
				Ref:            "institution-fonts-v1",
				Directory:      directory,
				Kind:           "font",
				Owners:         []string{"alice"},
				Redistribution: "deny",
			},
		},
	)
	request := model.ResolveRequest{
		Resources: []model.Resource{{ID: "fonts", Type: "server", Ref: "institution-fonts-v1", Mount: "fonts"}},
	}
	serverEnv, err := manager.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(serverEnv.Missing) != 0 || serverEnv.Resources[0].Redistribution != "deny" {
		t.Fatalf("server compile grant was not applied: %#v", serverEnv)
	}
	if _, err := manager.Prepare(context.Background(), "alice", serverEnv.Digest); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Pin("alice", serverEnv.Digest)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	file := serverEnv.Resources[0].Files[0]
	if opened, _, err := manager.OpenExport(
		"alice",
		serverEnv.Digest,
		"fonts",
		file.SHA256,
	); !errors.Is(
		err,
		ErrExport,
	) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("private catalog resource export was authorized: %v", err)
	}
	// The manifest is visible to a permitted compiler. Knowing that manifest is
	// not proof that the owner possesses the private bytes for a file upload.
	forged := model.ResolveRequest{
		Resources: []model.Resource{
			{
				ID:             "fonts",
				Type:           "files",
				Kind:           "font",
				Mount:          "fonts",
				Files:          []model.File{file},
				Redistribution: "allow",
			},
		},
	}
	env, err := manager.Resolve(context.Background(), "alice", forged)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Missing) != 1 || env.Missing[0] != file.SHA256 || env.Ready {
		t.Fatalf("catalog grant became uploaded proof: %#v", env)
	}
	if opened, _, err := manager.OpenExport("alice", env.Digest, "fonts", file.SHA256); !errors.Is(err, ErrExport) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("forged file export was authorized: %v", err)
	}
	if _, err := manager.Resolve(context.Background(), "bob", request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("catalog owner restriction was not enforced: %v", err)
	}
}

func TestSameEnvironmentDigestKeepsOwnerExportPermissionsIndependent(t *testing.T) {
	manager := newReviewManager(t, nil)
	contents := "font file supplied by each owner"
	file := reviewFile("Paper.otf", contents)
	request := model.ResolveRequest{
		Resources: []model.Resource{
			{
				ID:             "fonts",
				Type:           "files",
				Kind:           "font",
				Mount:          "fonts",
				Files:          []model.File{file},
				Redistribution: "deny",
			},
		},
	}
	alice, err := manager.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.PutBlob(
		context.Background(),
		"alice",
		alice.Digest,
		file.SHA256,
		strings.NewReader(contents),
	); err != nil {
		t.Fatal(err)
	}
	request.Resources[0].Redistribution = "allow"
	bob, err := manager.Resolve(context.Background(), "bob", request)
	if err != nil {
		t.Fatal(err)
	}
	if alice.Digest != bob.Digest || len(bob.Missing) != 1 {
		t.Fatalf("environment identity and possession were conflated: alice=%#v bob=%#v", alice, bob)
	}
	if err := manager.PutBlob(
		context.Background(),
		"bob",
		bob.Digest,
		file.SHA256,
		strings.NewReader(contents),
	); err != nil {
		t.Fatal(err)
	}
	request.Resources[0].Redistribution = "deny"
	if _, err := manager.Resolve(context.Background(), "alice", request); err != nil {
		t.Fatal(err)
	}
	if opened, _, err := manager.OpenExport("alice", alice.Digest, "fonts", file.SHA256); !errors.Is(err, ErrExport) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("another owner's export policy replaced alice's: %v", err)
	}
	opened, _, err := manager.OpenExport("bob", bob.Digest, "fonts", file.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	data, err := io.ReadAll(opened)
	if err != nil || string(data) != contents {
		t.Fatalf("bob's independent export grant was changed: %q %v", data, err)
	}
}

func TestCatalogRevocationAppliesToExistingEnvironmentGrants(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "Paper.otf"), []byte("font"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := newReviewManager(
		t,
		[]catalogResource{
			{Ref: "fonts-v1", Directory: directory, Kind: "font", Redistribution: "allow", Owners: []string{"alice"}},
		},
	)
	env, err := manager.Resolve(
		context.Background(),
		"alice",
		model.ResolveRequest{
			Resources: []model.Resource{{ID: "fonts", Type: "server", Ref: "fonts-v1", Mount: "fonts"}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), "alice", env.Digest); err != nil {
		t.Fatal(err)
	}
	file := env.Resources[0].Files[0]
	manager.mu.Lock()
	entry := manager.catalog["fonts-v1"]
	entry.Redistribution = "deny"
	manager.catalog[entry.Ref] = entry
	manager.mu.Unlock()
	if opened, _, err := manager.OpenExport("alice", env.Digest, "fonts", file.SHA256); !errors.Is(err, ErrExport) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("a revoked redistribution grant remained usable: %v", err)
	}
	manager.mu.Lock()
	entry.Owners = []string{"bob"}
	manager.catalog[entry.Ref] = entry
	manager.mu.Unlock()
	if _, err := manager.Pin("alice", env.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked catalog owner could pin the old environment: %v", err)
	}
	if _, err := manager.Inspect("alice", env.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked catalog owner could inspect the old environment: %v", err)
	}
}
