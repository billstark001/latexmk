package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourcePolicyRejectsUnsafeOrAmbiguousTransport(t *testing.T) {
	for _, policy := range []SourcePolicy{
		{Version: 0, ArchiveMirrors: []string{"https://mirror.example"}},
		{Version: 1},
		{Version: 1, ArchiveMirrors: []string{"http://mirror.example"}},
		{Version: 1, ArchiveMirrors: []string{"https://user:secret@mirror.example"}},
		{Version: 1, ArchiveMirrors: []string{"https://mirror.example?token=secret"}},
		{Version: 1, ArchiveMirrors: []string{"https://mirror.example", "https://mirror.example/"}},
		{Version: 1, ArchiveMirrors: []string{"https://mirror.example"}, PerSourceTimeout: "0s"},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("accepted invalid policy: %#v", policy)
		}
	}
}

func TestPinnedIndexRequiresExactSnapshotAndBytes(t *testing.T) {
	directory := t.TempDir()
	data := []byte("name algorithmicx\nrevision 15878\nrelocated 1\n")
	hash := sha256.Sum256(data)
	path := filepath.Join(directory, "remote.tlpdb")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	baseline := Baseline{Repository: "https://snapshot.example/2026/09/09/tlnet", TexliveYear: 2026}
	index := PinnedPackageIndex{
		Version:     1,
		Path:        path,
		SHA256:      hex.EncodeToString(hash[:]),
		Repository:  baseline.Repository,
		TexliveYear: 2026,
	}
	actual, err := index.Read(baseline, 4096)
	if err != nil || string(actual) != string(data) {
		t.Fatalf("trusted index rejected: %q %v", actual, err)
	}
	for _, changed := range []Baseline{
		{Repository: "https://snapshot.example/2026/09/10/tlnet", TexliveYear: 2026},
		{Repository: baseline.Repository, TexliveYear: 2025},
	} {
		_, err := index.Read(changed, 4096)
		var failure *AcquisitionError
		if !errors.As(err, &failure) || failure.Code != "resource_index_invalid" || failure.Reason != "binding" {
			t.Fatalf("snapshot mismatch accepted: %#v %v", failure, err)
		}
	}
	if err := os.WriteFile(path, append(data, '!'), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = index.Read(baseline, 4096)
	var failure *AcquisitionError
	if !errors.As(err, &failure) || failure.Reason != "checksum" || failure.Expected != index.SHA256 ||
		strings.Contains(err.Error(), directory) {
		t.Fatalf("mutated or leaking index: %#v %v", failure, err)
	}
}

func TestLoadPolicyResolvesPrivateIndexPathWithoutChangingSnapshot(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "sources.json")
	policy := SourcePolicy{Version: 1, ArchiveMirrors: []string{"https://mirror.example/tlnet-archive"},
		PackageIndex: &PinnedPackageIndex{Version: 1, Path: "remote.tlpdb", SHA256: strings.Repeat("a", 64),
			Repository: "https://snapshot.example/2026/09/09/tlnet", TexliveYear: 2026}}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSourcePolicy(path)
	if err != nil || loaded.PackageIndex.Path != filepath.Join(directory, "remote.tlpdb") ||
		loaded.PackageIndex.Repository != policy.PackageIndex.Repository {
		t.Fatalf("wrong resolved index assertion: %#v %v", loaded, err)
	}
}
