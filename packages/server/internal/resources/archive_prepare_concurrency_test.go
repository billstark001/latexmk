package resources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// completedPackageTree represents the output of runtime installation, keeping
// these concurrency tests independent of a locally installed TeX toolchain.
func completedPackageTree(t *testing.T, m *Manager) preparedTree {
	t.Helper()
	directory, err := os.MkdirTemp(filepath.Join(m.dir, "staging"), "completed-tree-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "installed.sty"), []byte("installed macro"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := inventoryFiles(directory, m.cfg.MaxResourceBytes, m.cfg.MaxResourceFiles)
	if err != nil {
		t.Fatal(err)
	}
	return preparedTree{directory: directory, files: files}
}

func awaitRetentionReference(t *testing.T, m *Manager, digest string) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		held := m.records[digest].refs > 0
		m.mu.Unlock()
		if held {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("archive operation did not retain the environment")
		}
	}
}

func TestArchiveRetentionDoesNotPreventCompletedTreePublication(t *testing.T) {
	for _, operation := range []string{"slow PUT", "waiting GET"} {
		t.Run(operation, func(t *testing.T) {
			m := testManager(t)
			env := archiveEnvironment(t, m, []byte("archive"))
			checksum := env.Packages[0].Checksum
			if err := m.PutPackageArchive(
				context.Background(),
				"alice",
				env.Digest,
				checksum,
				7,
				strings.NewReader("archive"),
			); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			var release func()
			if operation == "slow PUT" {
				reader := &gatedArchiveReader{
					started: make(chan struct{}),
					release: make(chan struct{}),
					body:    strings.NewReader("archive"),
				}
				go func() { finished <- m.PutPackageArchive(ctx, "alice", env.Digest, checksum, 7, reader) }()
				<-reader.started
				release = func() { close(reader.release) }
			} else {
				// A completed preparation owns one admission slot; another active
				// operation occupies the other. GET retains its record while queued.
				for range cap(m.prepares) {
					m.prepares <- struct{}{}
				}
				go func() { _, err := m.PackageArchives(ctx, "alice", env.Digest); finished <- err }()
				awaitRetentionReference(t, m, env.Digest)
				release = func() {
					for range cap(m.prepares) {
						<-m.prepares
					}
				}
			}
			tree := completedPackageTree(t, m)
			defer func() { _ = tree.remove() }()
			m.mu.Lock()
			rec := m.records[env.Digest]
			rec.refs++ // The completed Prepare call's own retention reference.
			if rec.refs != 2 || rec.treeLeases != 0 {
				m.mu.Unlock()
				release()
				<-finished
				t.Fatal("archive operation was not held at the publication boundary")
			}
			err := m.publishPreparedLocked(rec, rec.Owners[ownerKey("alice")], tree)
			rec.refs--
			m.mu.Unlock()
			release()
			if operationErr := <-finished; operationErr != nil {
				t.Fatal(operationErr)
			}
			if err != nil {
				t.Fatalf("archive retention was mistaken for a compile tree lease: %v", err)
			}
			lease, err := m.Pin("alice", env.Digest)
			if err != nil {
				t.Fatalf("concurrently published tree was not usable: %v", err)
			}
			lease.Release()
			lease.Release()
			m.mu.Lock()
			refs, treeLeases := rec.refs, rec.treeLeases
			m.mu.Unlock()
			if refs != 0 || treeLeases != 0 {
				t.Fatalf("archive/publication/compile leaked leases: refs=%d tree=%d", refs, treeLeases)
			}
		})
	}
}

func TestActiveCompileTreeLeaseStillPreventsReplacement(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("macro", "texmf")
	request.Resources[0].Mount = "."
	request.Resources[0].Files[0].Path = "paper.sty"
	env := uploadResource(t, m, "alice", request, "macro")
	lease, err := m.Pin("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	replacement := completedPackageTree(t, m)
	defer func() { _ = replacement.remove() }()
	m.mu.Lock()
	rec := m.records[env.Digest]
	err = m.publishPreparedLocked(rec, rec.Owners[ownerKey("alice")], replacement)
	treeLeases := rec.treeLeases
	m.mu.Unlock()
	if err == nil || treeLeases != 1 {
		t.Fatalf("active compile tree was replaceable: leases=%d error=%v", treeLeases, err)
	}
	data, err := os.ReadFile(filepath.Join(lease.Tree(), "paper.sty"))
	if err != nil || string(data) != "macro" {
		t.Fatalf("rejected replacement changed leased tree: %q %v", data, err)
	}
}
