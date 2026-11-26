package resourcecache

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/resources"
)

func archiveFixture(value string) ([]byte, resources.Package) {
	data := append([]byte{0xfd, '7', 'z', 'X', 'Z', 0}, []byte(value)...)
	digest := sha512.Sum512(data)
	return data, resources.Package{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(digest[:])}
}

func TestArchiveCacheFallbackHitAndCorruptionRepair(t *testing.T) {
	data, pkg := archiveFixture("verified archive")
	var downloads atomic.Int32
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>challenge</html>")
	}))
	defer bad.Close()
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if r.URL.Path != "/archive/algorithms.r42.tar.xz" {
			t.Errorf("unexpected archive URL %s", r.URL.Path)
		}
		_, _ = w.Write(data)
	}))
	defer good.Close()
	cache, err := Open(Settings{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	options := resources.ArchiveFetchOptions{Client: good.Client(), Package: pkg, MaxBytes: 1024,
		Policy: &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{bad.URL, good.URL}}}
	for i := range 2 {
		archive, err := cache.Acquire(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(archive.File)
		if err != nil || string(got) != string(data) {
			t.Fatalf("invalid cached bytes: %q %v", got, err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if downloads.Load() != 1 {
			t.Fatalf("cache hit downloaded again: %d (%d)", downloads.Load(), i)
		}
	}
	name := filepath.Join(cache.directory, "archives", pkg.Checksum+".tar.xz")
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	archive, err := cache.Acquire(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 2 {
		t.Fatal("corrupt archive was not repaired")
	}
	entries, _ := os.ReadDir(filepath.Dir(name))
	if len(entries) != 1 {
		t.Fatalf("failed sources left staging files: %v", entries)
	}
}

func TestArchiveCacheConcurrentAcquisitionAndCancellation(t *testing.T) {
	data, pkg := archiveFixture("one download")
	var downloads atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	cache, err := Open(Settings{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	options := resources.ArchiveFetchOptions{Client: server.Client(), Package: pkg, MaxBytes: 1024,
		Policy: &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{server.URL}}}
	var workers sync.WaitGroup
	for range 6 {
		workers.Go(func() {
			archive, err := cache.Acquire(context.Background(), options)
			if err != nil {
				t.Error(err)
				return
			}
			if err := archive.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if downloads.Load() != 1 {
		t.Fatalf("concurrent cache miss downloaded %d times", downloads.Load())
	}
	held, err := cache.Acquire(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Acquire(ctx, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting ignored cancellation: %v", err)
	}
	info, err := cache.Prune(context.Background(), true)
	if err != nil || info.Archives != 1 {
		t.Fatalf("prune removed a leased archive: %#v %v", info, err)
	}
}

func TestArchiveCacheEvictsOnlyUnusedObjectsWithinBudget(t *testing.T) {
	data, pkg := archiveFixture("first object")
	other, otherPkg := archiveFixture("second object")
	otherPkg.Name = "algorithmicx"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive/algorithms.r42.tar.xz" {
			_, _ = w.Write(data)
		} else {
			_, _ = w.Write(other)
		}
	}))
	defer server.Close()
	cache, err := Open(Settings{Directory: t.TempDir(), MaxBytes: int64(len(data) + len(other) - 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	options := resources.ArchiveFetchOptions{Client: server.Client(), Package: pkg, MaxBytes: 1024,
		Policy: &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{server.URL}}}
	first, err := cache.Acquire(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	options.Package = otherPkg
	if _, err := cache.Acquire(context.Background(), options); err == nil {
		t.Fatal("evicted an archive being uploaded")
	} else {
		var capacity *resources.AcquisitionError
		if !errors.As(err, &capacity) || capacity.Code != "resource_cache_unavailable" || !capacity.Retryable {
			t.Fatalf("leased cache capacity misclassified: %#v %v", capacity, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := cache.Acquire(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := cache.Info()
	if err != nil || info.Archives != 1 || info.Bytes != int64(len(other)) {
		t.Fatalf("bad quota accounting: %#v %v", info, err)
	}
}

func TestResourceCacheDirectoryConfinement(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "paper")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{root, project, "relative/cache"} {
		if _, err := Directory(name, project); err == nil {
			t.Fatalf("unsafe cache accepted: %s", name)
		}
	}
	want := filepath.Join(project, "cache")
	got, err := Directory(want, project)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := canonicalPath(want)
	if got != canonical {
		t.Fatalf("cache path mismatch: %s", got)
	}
}

func TestRestrictiveLimitDoesNotDeleteValidSharedArchive(t *testing.T) {
	data, pkg := archiveFixture("shared archive retained")
	var calls atomic.Int32
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write(data) }),
	)
	defer server.Close()
	directory := t.TempDir()
	cache, err := Open(Settings{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	options := resources.ArchiveFetchOptions{Client: server.Client(), Package: pkg, MaxBytes: 1024,
		Policy: &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{server.URL}}}
	archive, err := cache.Acquire(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	limited := options
	limited.MaxBytes = int64(len(data) - 1)
	if _, err := cache.Acquire(context.Background(), limited); err == nil {
		t.Fatal("target limit bypassed")
	}
	small, err := Open(Settings{Directory: directory, MaxBytes: int64(len(data) - 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = small.Close() }()
	if _, err := small.Acquire(context.Background(), options); err == nil {
		t.Fatal("reduced user cache budget bypassed")
	}
	archive, err = cache.Acquire(context.Background(), options)
	if err != nil {
		t.Fatal("restrictive reader destroyed shared archive", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("restriction caused re-download: %d", calls.Load())
	}
}
