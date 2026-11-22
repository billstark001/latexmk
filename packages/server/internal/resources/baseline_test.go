package resources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func registeredBaselineForIdentityTest() model.Baseline {
	return model.Baseline{
		ID: "target", Image: "registry.example/target@sha256:" + strings.Repeat("b", 64),
		TexliveYear: 2026, Repository: "https://snapshot.example/2026/09/09/tlnet",
		Engines: []string{"xelatex"}, FontLookup: "names", Toolchain: map[string]string{"xelatex": "XeTeX 2026"},
		Fonts:    []model.Font{{File: "Paper.otf", SHA256: strings.Repeat("c", 64), Family: "Paper", Style: "Regular"}},
		Packages: []model.Package{{Name: "algorithms", Revision: 42, Checksum: strings.Repeat("d", 128)}},
	}
}

func newBaselineIdentityManager(t *testing.T, baseline model.Baseline) (*Manager, error) {
	t.Helper()
	registryFile := filepath.Join(t.TempDir(), "registry.json")
	data, err := json.Marshal(registry{Baselines: []model.Baseline{baseline}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	return New(config.Config{
		StateDir: t.TempDir(), ResourceRegistryFile: registryFile, ResourceInventoryDir: t.TempDir(),
		RunnerImage: "registry.example/controller@sha256:" + strings.Repeat("a", 64), Engines: []string{"xelatex"},
		MaxResourceBytes: 64 << 10, MaxResourceFiles: 100, ResourceCacheBytes: 2 << 20, ResourceOwnerBytes: 1 << 20,
	}, protocol.Metadata{})
}

func TestRegisteredBaselineComputesMissingDigestAndVerifiesExplicitDigest(t *testing.T) {
	baseline := registeredBaselineForIdentityTest()
	expected, err := model.BaselineDigest(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, digest string
		wantErr      bool
	}{
		{"omitted", "", false},
		{"correct", expected, false},
		{"wrong", strings.Repeat("f", 64), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := copyBaseline(baseline)
			candidate.Digest = test.digest
			manager, err := newBaselineIdentityManager(t, candidate)
			if (err != nil) != test.wantErr {
				t.Fatalf("registration error: %v", err)
			}
			if test.wantErr {
				return
			}
			found := false
			for _, registered := range manager.List() {
				if registered.ID == baseline.ID {
					found = true
					if registered.Digest != expected {
						t.Fatalf(
							"registry did not bind the complete baseline body: %s versus %s",
							registered.Digest,
							expected,
						)
					}
				}
			}
			if !found {
				t.Fatal("registered baseline was not listed")
			}
		})
	}
}

func TestRegisteredRuntimeChangesAffectEnvironmentDigestUnderTheSameID(t *testing.T) {
	baseline := registeredBaselineForIdentityTest()
	resolve := func(candidate model.Baseline) model.Environment {
		t.Helper()
		manager, err := newBaselineIdentityManager(t, candidate)
		if err != nil {
			t.Fatal(err)
		}
		environment, err := manager.Resolve(context.Background(), "alice", model.ResolveRequest{Baseline: candidate.ID})
		if err != nil {
			t.Fatal(err)
		}
		return environment
	}
	before := resolve(baseline)
	for _, test := range []struct {
		name   string
		mutate func(*model.Baseline)
	}{
		{"image", func(value *model.Baseline) { value.Image = "registry.example/target@sha256:" + strings.Repeat("e", 64) }},
		{"font", func(value *model.Baseline) { value.Fonts[0].SHA256 = strings.Repeat("e", 64) }},
		{"package", func(value *model.Baseline) { value.Packages[0].Revision++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := copyBaseline(baseline)
			test.mutate(&candidate)
			after := resolve(candidate)
			if after.Baseline.ID != before.Baseline.ID || after.Baseline.Digest == before.Baseline.Digest ||
				after.Digest == before.Digest {
				t.Fatalf(
					"changed %s reused the old environment identity: before=%#v after=%#v",
					test.name,
					before,
					after,
				)
			}
		})
	}
}

func TestPinRejectsBaselineBodyTamperingWithUnchangedDigest(t *testing.T) {
	for _, location := range []string{"record", "owner-grant", "registry"} {
		t.Run(location, func(t *testing.T) {
			baseline := registeredBaselineForIdentityTest()
			manager, err := newBaselineIdentityManager(t, baseline)
			if err != nil {
				t.Fatal(err)
			}
			environment, err := manager.Resolve(
				context.Background(),
				"alice",
				model.ResolveRequest{Baseline: baseline.ID},
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Prepare(context.Background(), "alice", environment.Digest); err != nil {
				t.Fatal(err)
			}
			manager.mu.Lock()
			record := manager.records[environment.Digest]
			if !record.Prepared {
				manager.mu.Unlock()
				t.Fatal("test did not reach the prepared environment state")
			}
			changedImage := "registry.example/replaced@sha256:" + strings.Repeat("e", 64)
			switch location {
			case "record":
				record.Environment.Baseline.Image = changedImage
			case "owner-grant":
				record.Owners[ownerKey("alice")].Environment.Baseline.Image = changedImage
			case "registry":
				changed := manager.baselines[baseline.ID]
				changed.Image = changedImage
				manager.baselines[baseline.ID] = changed
			}
			manager.mu.Unlock()
			lease, err := manager.Pin("alice", environment.Digest)
			if err == nil {
				lease.Release()
				t.Fatal("pin trusted an unchanged Digest over a changed runtime body")
			}
		})
	}
}
