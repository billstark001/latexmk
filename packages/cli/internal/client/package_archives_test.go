package client

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/resources"
)

func packageArchiveClientFixture(t *testing.T) (resources.Environment, resources.PackageArchives, []byte) {
	t.Helper()
	data := append([]byte{0xfd, '7', 'z', 'X', 'Z', 0}, []byte("locked package archive")...)
	sum := sha512.Sum512(data)
	packages := []resources.Package{
		{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(sum[:])},
		{Name: "algorithmicx", Revision: 43, Checksum: strings.Repeat("b", 128)},
	}
	environment := resources.Environment{Baseline: resources.Baseline{
		ID:          "default",
		Digest:      strings.Repeat("a", 64),
		Repository:  "https://snapshot.example/2026/09/09/tlnet",
		TexliveYear: 2026,
		Engines:     []string{"xelatex"},
	}, Packages: packages}
	var err error
	environment.Digest, err = resources.EnvironmentDigest(environment)
	if err != nil {
		t.Fatal(err)
	}
	manifest := resources.PackageArchives{
		EnvironmentDigest: environment.Digest,
		Archives: []resources.PackageArchive{
			{Package: packages[0], MaxBytes: 1024},
			{Package: packages[1], Present: true, MaxBytes: 1024},
		},
		Sources: &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{"https://mirror.example/tlnet"}},
	}
	return environment, manifest, data
}

func TestPackageArchivesValidateExactResolvedManifestBeforeAcquisition(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*resources.PackageArchives)
		valid  bool
	}{
		{"exact", func(*resources.PackageArchives) {}, true},
		{"reordered", func(m *resources.PackageArchives) { m.Archives[0], m.Archives[1] = m.Archives[1], m.Archives[0] }, true},
		{"legacy sources omitted", func(m *resources.PackageArchives) { m.Sources = nil }, true},
		{"wrong environment", func(m *resources.PackageArchives) { m.EnvironmentDigest = strings.Repeat("f", 64) }, false},
		{"missing package", func(m *resources.PackageArchives) { m.Archives = m.Archives[:1] }, false},
		{"duplicate package", func(m *resources.PackageArchives) { m.Archives[1] = m.Archives[0] }, false},
		{"undeclared package", func(m *resources.PackageArchives) { m.Archives[0].Package.Name = "unlocked" }, false},
		{"unlocked revision", func(m *resources.PackageArchives) { m.Archives[0].Package.Revision++ }, false},
		{"unlocked checksum", func(m *resources.PackageArchives) { m.Archives[0].Package.Checksum = strings.Repeat("c", 128) }, false},
		{"zero byte limit", func(m *resources.PackageArchives) { m.Archives[0].MaxBytes = 0 }, false},
		{"unbounded byte limit", func(m *resources.PackageArchives) { m.Archives[0].MaxBytes = (1 << 40) + 1 }, false},
		{"local index leak", func(m *resources.PackageArchives) {
			m.Sources.PackageIndex = &resources.PinnedPackageIndex{Path: "/private/server-index"}
		}, false},
		{"insecure transport source", func(m *resources.PackageArchives) { m.Sources.ArchiveMirrors[0] = "http://mirror.example/tlnet" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment, manifest, _ := packageArchiveClientFixture(t)
			test.mutate(&manifest)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req_manifest_contract")
				calls.Add(1)
				if r.Method != http.MethodGet ||
					r.URL.Path != "/v1/environments/"+environment.Digest+"/package-archives" ||
					r.Header.Get("Authorization") != "Bearer compiler-token" {
					t.Errorf("unexpected manifest request: %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(manifest)
			}))
			defer server.Close()
			connected, err := New(server.URL, "compiler-token", 0, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = connected.PackageArchives(context.Background(), environment)
			if (err == nil) != test.valid {
				t.Fatalf("manifest valid=%v, error=%v", test.valid, err)
			}
			if !test.valid {
				var failure *HTTPError
				if !errors.As(err, &failure) || failure.StatusCode != http.StatusOK ||
					failure.RequestID != "req_manifest_contract" || failure.ResponseIssue != "invalid_api_response" {
					t.Fatalf("semantic response failure lost HTTP context: %#v %v", failure, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("unexpected follow-up requests: %d", calls.Load())
			}
		})
	}
	t.Run("invalid local environment makes no request", func(t *testing.T) {
		environment, _, _ := packageArchiveClientFixture(t)
		environment.Digest = strings.Repeat("f", 64)
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
		defer server.Close()
		connected, _ := New(server.URL, "", 0, false)
		if _, err := connected.PackageArchives(context.Background(), environment); err == nil || calls.Load() != 0 {
			t.Fatalf("invalid environment contacted server: calls=%d error=%v", calls.Load(), err)
		}
	})
}

func TestUploadPackageArchiveSendsExactLeasedBytesAndRequires204(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusOK, http.StatusRequestEntityTooLarge} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			environment, _, data := packageArchiveClientFixture(t)
			name := filepath.Join(t.TempDir(), "archive.tar.xz")
			if err := os.WriteFile(
				name,
				append(append([]byte{}, data...), []byte("unrelated trailing bytes")...),
				0600,
			); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			if _, err := file.Seek(0, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPut ||
					r.URL.Path != "/v1/environments/"+environment.Digest+"/package-archives/"+environment.Packages[0].Checksum ||
					r.ContentLength != int64(len(data)) ||
					len(r.TransferEncoding) != 0 ||
					r.Header.Get("Authorization") != "Bearer compiler-token" ||
					r.Header.Get("Content-Type") != "application/octet-stream" ||
					!bytes.Equal(body, data) {
					t.Errorf(
						"archive request changed leased bytes: method=%s path=%s length=%d body=%q",
						r.Method,
						r.URL.Path,
						r.ContentLength,
						body,
					)
				}
				w.WriteHeader(status)
				if status == http.StatusRequestEntityTooLarge {
					_, _ = io.WriteString(
						w,
						`{"error":"fixed resource limit","code":"resource_limit_exceeded","retryable":false}`,
					)
				}
			}))
			defer server.Close()
			connected, _ := New(server.URL, "compiler-token", 0, false)
			err = connected.UploadPackageArchive(
				context.Background(),
				environment.Digest,
				environment.Packages[0],
				file,
				int64(len(data)),
			)
			if (err == nil) != (status == http.StatusNoContent) {
				t.Fatalf("upload status%d accepted incorrectly: %v", status, err)
			}
			if _, err := file.Stat(); err != nil {
				t.Fatalf("client closed caller-owned cache lease: %v", err)
			}
		})
	}
}
