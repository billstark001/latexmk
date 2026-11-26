package resources

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func archiveEnvironment(t *testing.T, m *Manager, content []byte) model.Environment {
	t.Helper()
	sum := sha512.Sum512(content)
	env := model.Environment{
		Baseline:  m.List()[0],
		Resources: []model.Resource{{ID: "algorithms", Type: "texlive", Packages: []string{"algorithms"}}},
		Packages:  []model.Package{{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(sum[:])}},
	}
	var err error
	env.Digest, err = model.EnvironmentDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	rec := &record{Environment: env, Owners: map[string]*ownerAccess{
		ownerKey("alice"): {Environment: env, Uploaded: map[string]bool{}, Used: time.Now().UTC()},
	}}
	m.records[env.Digest] = rec
	err = m.saveLocked(rec)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestPackageArchiveUploadIsAuthorizedVerifiedAndIdentityIndependent(t *testing.T) {
	m := testManager(t)
	content := []byte("an archive selected by the authoritative resolver")
	env := archiveEnvironment(t, m, content)
	before, _ := json.Marshal(env)
	manifest, err := m.PackageArchives(context.Background(), "alice", env.Digest)
	if err != nil || len(manifest.Archives) != 1 || manifest.Archives[0].Present || manifest.Sources != nil {
		t.Fatalf("initial availability: %+v %v", manifest, err)
	}
	checksum := env.Packages[0].Checksum
	for _, test := range []struct{ owner, digest, checksum string }{
		{"bob", env.Digest, checksum}, {"alice", strings.Repeat("b", 64), checksum},
		{"alice", env.Digest, "invalid"}, {"alice", env.Digest, strings.Repeat("a", 128)},
	} {
		if err := m.PutPackageArchive(
			context.Background(),
			test.owner,
			test.digest,
			test.checksum,
			int64(len(content)),
			bytes.NewReader(content),
		); !errors.Is(
			err,
			ErrNotFound,
		) {
			t.Fatalf("unbound upload accepted: %+v %v", test, err)
		}
	}
	for range 2 {
		if err := m.PutPackageArchive(
			context.Background(),
			"alice",
			env.Digest,
			checksum,
			int64(len(content)),
			bytes.NewReader(content),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.PutPackageArchive(
		context.Background(),
		"alice",
		env.Digest,
		checksum,
		5,
		strings.NewReader("wrong"),
	); !errors.Is(
		err,
		ErrArchiveChecksum,
	) {
		t.Fatalf("cached bytes hid bad request body: %v", err)
	}
	manifest, err = m.PackageArchives(context.Background(), "alice", env.Digest)
	if err != nil || !manifest.Archives[0].Present || manifest.Archives[0].Package != env.Packages[0] ||
		manifest.Archives[0].MaxBytes != m.cfg.MaxResourceBytes {
		t.Fatalf("uploaded availability: %+v %v", manifest, err)
	}
	if _, err := m.PackageArchives(context.Background(), "bob", env.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive manifest leaked to another owner: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.dir, "archives", checksum+".tar.xz"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("cache contents changed: %q %v", got, err)
	}
	after, _ := json.Marshal(m.records[env.Digest].Environment)
	if !bytes.Equal(before, after) || m.reserved != 0 || m.records[env.Digest].refs != 0 {
		t.Fatalf(
			"upload changed identity or leaked reservation/ref: reserved=%d refs=%d",
			m.reserved,
			m.records[env.Digest].refs,
		)
	}
}

func TestPackageArchiveManifestCopiesTransportPolicyWithoutLocalIndex(t *testing.T) {
	m := testManager(t)
	env := archiveEnvironment(t, m, []byte("archive"))
	m.cfg.ResourceSourcePolicy = &model.SourcePolicy{
		Version:        1,
		ArchiveMirrors: []string{"https://mirror.example/tlnet"},
		PackageIndex:   &model.PinnedPackageIndex{Path: "/private/server-index.tlpdb"},
	}
	manifest, err := m.PackageArchives(context.Background(), "alice", env.Digest)
	if err != nil || manifest.Sources == nil || manifest.Sources.PackageIndex != nil {
		t.Fatalf("private policy exposed: %+v %v", manifest, err)
	}
	manifest.Sources.ArchiveMirrors[0] = "changed"
	if m.cfg.ResourceSourcePolicy.ArchiveMirrors[0] != "https://mirror.example/tlnet" {
		t.Fatal("manifest shares mutable policy state")
	}
	for range cap(m.prepares) {
		m.prepares <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.PackageArchives(ctx, "alice", env.Digest); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("archive hashing admission ignored timeout: %v", err)
	}
	for range cap(m.prepares) {
		<-m.prepares
	}
	if m.records[env.Digest].refs != 0 {
		t.Fatal("cancelled archive manifest retained an environment lease")
	}
}

func TestPackageArchiveUploadLimitsCancellationAndCorruptCacheRecovery(t *testing.T) {
	for _, test := range []struct {
		name            string
		limit, declared int64
		body            string
		want            error
	}{
		{"known oversized", 3, 7, "archive", ErrArchiveSize},
		{"stream oversized", 3, -1, "archive", ErrArchiveSize},
		{"short content length", 20, 3, "archive", ErrArchiveSize},
		{"long content length", 20, 10, "archive", ErrArchiveSize},
		{"checksum", 20, 7, "invalid", ErrArchiveChecksum},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := testManager(t)
			env := archiveEnvironment(t, m, []byte("archive"))
			m.cfg.MaxResourceBytes = test.limit
			err := m.PutPackageArchive(
				context.Background(),
				"alice",
				env.Digest,
				env.Packages[0].Checksum,
				test.declared,
				strings.NewReader(test.body),
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			entries, _ := os.ReadDir(filepath.Join(m.dir, "archives"))
			if len(entries) != 0 || m.reserved != 0 || m.records[env.Digest].refs != 0 {
				t.Fatalf("failed upload retained bytes or reservations: %v", entries)
			}
		})
	}
	m := testManager(t)
	env := archiveEnvironment(t, m, []byte("archive"))
	checksum := env.Packages[0].Checksum
	release, err := acquirePackageArchive(context.Background(), filepath.Join(m.dir, "archives"), checksum)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- m.PutPackageArchive(ctx, "alice", env.Digest, checksum, 7, strings.NewReader("archive"))
	}()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("upload ignored cancellation while waiting for archive")
	}
	release()
	path := filepath.Join(m.dir, "archives", checksum+".tar.xz")
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := m.PackageArchives(context.Background(), "alice", env.Digest)
	if err != nil || manifest.Archives[0].Present {
		t.Fatalf("corruption accepted: %+v %v", manifest, err)
	}
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
}

type gatedArchiveReader struct {
	started chan struct{}
	release chan struct{}
	body    *strings.Reader
}

func (r *gatedArchiveReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
		<-r.release
	}
	return r.body.Read(p)
}

func TestPackageArchiveUploadReservesConcurrentBytesAndDeduplicates(t *testing.T) {
	m := testManager(t)
	env := archiveEnvironment(t, m, []byte("archive"))
	other := archiveEnvironment(t, m, []byte("another"))
	used, err := diskBytes(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.ResourceCacheBytes = used + 7
	reader := &gatedArchiveReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		body:    strings.NewReader("archive"),
	}
	finished := make(chan error, 1)
	go func() {
		finished <- m.PutPackageArchive(context.Background(), "alice", env.Digest, env.Packages[0].Checksum, 7, reader)
	}()
	<-reader.started
	err = m.PutPackageArchive(
		context.Background(),
		"alice",
		other.Digest,
		other.Packages[0].Checksum,
		7,
		strings.NewReader("another"),
	)
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("concurrent upload exceeded cache capacity: %v", err)
	}
	var unavailable *model.AcquisitionError
	if !errors.As(err, &unavailable) || unavailable.Code != "resource_cache_unavailable" || !unavailable.Retryable ||
		unavailable.FallbackAllowed {
		t.Fatalf("temporary reservation exhaustion was not classified: %v", err)
	}
	close(reader.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	// Idempotent uploads need no additional disk reservation once bytes exist.
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := m.PutPackageArchive(
				context.Background(),
				"alice",
				env.Digest,
				env.Packages[0].Checksum,
				7,
				strings.NewReader("archive"),
			); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	if m.reserved != 0 || m.records[env.Digest].refs != 0 {
		t.Fatal("concurrent upload leaked a reservation or lease")
	}
}

func TestResourceReservationDistinguishesConfiguredLimitFromTemporaryCapacity(t *testing.T) {
	m := testManager(t)
	m.cfg.ResourceCacheBytes = 16
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reserveLocked(17); !errors.Is(err, ErrQuota) {
		t.Fatalf("fixed capacity limit: %v", err)
	} else {
		var failure *model.AcquisitionError
		if errors.As(err, &failure) {
			t.Fatalf("fixed capacity limit marked temporary: %+v", failure)
		}
	}
	if err := m.reserveLocked(10); err != nil {
		t.Fatal(err)
	}
	err := m.reserveLocked(7)
	var failure *model.AcquisitionError
	if !errors.As(err, &failure) || failure.Code != "resource_cache_unavailable" || failure.HTTPStatus() != 503 ||
		failure.Stage != "resource-cache" ||
		failure.FallbackAllowed ||
		!failure.Retryable {
		t.Fatalf("temporary capacity exhaustion: %+v %v", failure, err)
	}
	m.reserved -= 10
	if err := m.reserveLocked(7); err != nil {
		t.Fatalf("released capacity did not recover: %v", err)
	}
	m.reserved -= 7
}
