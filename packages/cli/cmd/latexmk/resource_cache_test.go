package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/resourcecache"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func resourceArchiveCommandFixture(t *testing.T) (resources.Environment, []byte) {
	t.Helper()
	data := append([]byte{0xfd, '7', 'z', 'X', 'Z', 0}, []byte("one public locked archive")...)
	sum := sha512.Sum512(data)
	environment := resources.Environment{Baseline: resources.Baseline{ID: "default", Digest: strings.Repeat("a", 64),
		Repository: "https://snapshot.example/2026/09/09/tlnet", TexliveYear: 2026, Engines: []string{"xelatex"}},
		Resources: []resources.Resource{{ID: "algorithms", Type: "texlive", Packages: []string{"algorithms"}}},
		Packages:  []resources.Package{{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(sum[:])}}}
	var err error
	environment.Digest, err = resources.EnvironmentDigest(environment)
	if err != nil {
		t.Fatal(err)
	}
	return environment, data
}

func TestResourceAcquisitionModesPreserveServerPreparationWhenCacheFails(t *testing.T) {
	for _, mode := range []string{"auto", "client", "server"} {
		t.Run(mode, func(t *testing.T) {
			environment, _ := resourceArchiveCommandFixture(t)
			cacheDirectory := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(cacheDirectory, "archives"),
				[]byte("not a directory"),
				0600,
			); err != nil {
				t.Fatal(err)
			}
			var manifests, prepares atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/environments/resolve":
					_ = json.NewEncoder(w).Encode(environment)
				case strings.HasSuffix(r.URL.Path, "/package-archives"):
					manifests.Add(1)
					_ = json.NewEncoder(w).Encode(resources.PackageArchives{EnvironmentDigest: environment.Digest,
						Archives: []resources.PackageArchive{{Package: environment.Packages[0], MaxBytes: 1024}}})
				case strings.HasSuffix(r.URL.Path, "/prepare"):
					prepares.Add(1)
					ready := environment
					ready.Ready = true
					_ = json.NewEncoder(w).Encode(ready)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			connected, _ := client.New(server.URL, "", 0, false)
			opts := compileOptions{
				projectRoot:   t.TempDir(),
				engine:        "xelatex",
				resourceCache: resourcecache.Settings{Directory: cacheDirectory, Acquisition: mode},
			}
			var ready resources.Environment
			var err error
			_, _, stderr := captureCommandOutput(t, func() int {
				ready, err = resolveResourceEnvironment(
					context.Background(),
					connected,
					opts,
					protocol.Metadata{
						Capabilities: protocol.Capabilities{ResourceEnvironments: true, ResourceArchiveUploads: true},
					},
					nil,
					"default",
					resources.ResolveRequest{Resources: environment.Resources},
					resourcePrepare,
				)
				return 0
			})
			if mode == "client" {
				var failure *resources.AcquisitionError
				if !errors.As(err, &failure) || failure.Code != "resource_storage_failed" || prepares.Load() != 0 {
					t.Fatalf("client mode did not fail strictly: %#v %v prepares=%d", failure, err, prepares.Load())
				}
			} else if err != nil || !ready.Ready || prepares.Load() != 1 {
				t.Fatalf(
					"server preparation blocked by local cache: ready=%v err=%v prepares=%d",
					ready.Ready,
					err,
					prepares.Load(),
				)
			}
			if (mode == "server") != (manifests.Load() == 0) ||
				(mode == "auto") != strings.Contains(stderr, "trying server preparation") {
				t.Fatalf(
					"mode did not control acquisition/warnings: mode=%s manifests=%d stderr=%s",
					mode,
					manifests.Load(),
					stderr,
				)
			}
		})
	}
}

func TestAutoResourceAcquisitionDoesNotSuppressUploadFailure(t *testing.T) {
	environment, data := resourceArchiveCommandFixture(t)
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "archives"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "archives", environment.Packages[0].Checksum+".tar.xz"),
		data,
		0600,
	); err != nil {
		t.Fatal(err)
	}
	var prepares atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(resources.PackageArchives{EnvironmentDigest: environment.Digest,
				Archives: []resources.PackageArchive{{Package: environment.Packages[0], MaxBytes: 1024}}})
		case strings.Contains(r.URL.Path, "/package-archives/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"archive upload forbidden"}`)
		default:
			prepares.Add(1)
		}
	}))
	defer server.Close()
	connected, _ := client.New(server.URL, "", 0, false)
	err := preparePackageArchives(
		context.Background(),
		connected,
		compileOptions{
			projectRoot:   t.TempDir(),
			resourceCache: resourcecache.Settings{Directory: directory, Acquisition: "auto"},
		},
		protocol.Metadata{Capabilities: protocol.Capabilities{ResourceArchiveUploads: true}},
		environment,
	)
	var failure *client.HTTPError
	if !errors.As(err, &failure) || failure.StatusCode != http.StatusForbidden || prepares.Load() != 0 {
		t.Fatalf("auto mode suppressed upload authorization error: %#v %v", failure, err)
	}
}

func TestResourceAcquisitionFlagOverridesConfiguredMode(t *testing.T) {
	opts := compileOptions{timeout: time.Minute, resourceCache: resourcecache.Settings{Acquisition: "server"}}
	if _, err := compileTestArguments(
		[]string{"--resource-acquisition", "client"},
		&opts,
	); err != nil ||
		opts.resourceCache.Acquisition != "client" {
		t.Fatalf("flag did not override configured mode: %s %v", opts.resourceCache.Acquisition, err)
	}
	if _, err := compileTestArguments([]string{"--resource-acquisition", "unknown"}, &opts); err == nil {
		t.Fatal("invalid acquisition flag accepted")
	}
}

func TestStrictClientAcquisitionRequiresArchiveUploadCapability(t *testing.T) {
	environment, _ := resourceArchiveCommandFixture(t)
	err := preparePackageArchives(context.Background(), nil,
		compileOptions{resourceCache: resourcecache.Settings{Acquisition: "client"}}, protocol.Metadata{}, environment)
	var unsupported *client.CapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("strict client mode silently used server acquisition: %v", err)
	}
}

func TestAutoResourceAcquisitionRecoversFromClientTransportFailure(t *testing.T) {
	for _, mode := range []string{"auto", "client"} {
		t.Run(mode, func(t *testing.T) {
			environment, data := resourceArchiveCommandFixture(t)
			mirror := httptest.NewUnstartedServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }),
			)
			// The normal acquisition client does not trust this test-only CA.
			mirror.Config.ErrorLog = log.New(io.Discard, "", 0)
			mirror.StartTLS()
			defer mirror.Close()
			var prepares atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/environments/resolve":
					_ = json.NewEncoder(w).Encode(environment)
				case strings.HasSuffix(r.URL.Path, "/package-archives"):
					_ = json.NewEncoder(w).Encode(resources.PackageArchives{EnvironmentDigest: environment.Digest,
						Sources:  &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{mirror.URL}},
						Archives: []resources.PackageArchive{{Package: environment.Packages[0], MaxBytes: 1024}}})
				case strings.HasSuffix(r.URL.Path, "/prepare"):
					prepares.Add(1)
					ready := environment
					ready.Ready = true
					_ = json.NewEncoder(w).Encode(ready)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			connected, _ := client.New(server.URL, "", 0, false)
			opts := compileOptions{
				projectRoot:   t.TempDir(),
				engine:        "xelatex",
				resourceCache: resourcecache.Settings{Directory: t.TempDir(), Acquisition: mode},
			}
			var ready resources.Environment
			var err error
			_, _, stderr := captureCommandOutput(t, func() int {
				ready, err = resolveResourceEnvironment(
					context.Background(),
					connected,
					opts,
					protocol.Metadata{
						Capabilities: protocol.Capabilities{ResourceEnvironments: true, ResourceArchiveUploads: true},
					},
					nil,
					"default",
					resources.ResolveRequest{Resources: environment.Resources},
					resourcePrepare,
				)
				return 0
			})
			if mode == "auto" {
				if err != nil || !ready.Ready || prepares.Load() != 1 ||
					!strings.Contains(stderr, "trying server preparation") {
					t.Fatalf(
						"auto did not recover through server: ready=%v err=%v requests=%d stderr=%s",
						ready.Ready,
						err,
						prepares.Load(),
						stderr,
					)
				}
			} else {
				var failure *resources.AcquisitionError
				if !errors.As(err, &failure) || failure.Code != "resource_upstream_tls" || prepares.Load() != 0 {
					t.Fatalf("client mode suppressed transport failure: %#v %v", failure, err)
				}
			}
		})
	}
}

func TestPreparePackageArchivesSkipsOldServersAndPresentArchivesWithoutCreatingCache(t *testing.T) {
	for _, capability := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "old server", true: "all archives already present"}[capability],
			func(t *testing.T) {
				environment, _ := resourceArchiveCommandFixture(t)
				cacheDirectory := filepath.Join(t.TempDir(), "not-created")
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if !capability || r.Method != http.MethodGet ||
						r.URL.Path != "/v1/environments/"+environment.Digest+"/package-archives" {
						t.Errorf("unexpected archive API call: %s %s", r.Method, r.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(resources.PackageArchives{
						EnvironmentDigest: environment.Digest,
						Archives: []resources.PackageArchive{
							{Package: environment.Packages[0], Present: true, MaxBytes: 1024},
						},
					})
				}))
				defer server.Close()
				connected, _ := client.New(server.URL, "", 0, false)
				err := preparePackageArchives(
					context.Background(),
					connected,
					compileOptions{
						projectRoot:   t.TempDir(),
						resourceCache: resourcecache.Settings{Directory: cacheDirectory},
					},
					protocol.Metadata{Capabilities: protocol.Capabilities{ResourceArchiveUploads: capability}},
					environment,
				)
				if err != nil {
					t.Fatal(err)
				}
				want := int32(0)
				if capability {
					want = 1
				}
				if calls.Load() != want {
					t.Fatalf("archive calls=%d want%d", calls.Load(), want)
				}
				if _, err := os.Stat(cacheDirectory); !os.IsNotExist(err) {
					t.Fatalf("unneeded local cache was created: %v", err)
				}
			},
		)
	}
}

func TestReadyEnvironmentDoesNotRequireArchivesOrLocalCache(t *testing.T) {
	environment, _ := resourceArchiveCommandFixture(t)
	environment.Ready = true
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("ready environment negotiated unnecessary archive request: %s", r.URL.Path)
	}))
	defer server.Close()
	connected, err := client.New(server.URL, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "not-created")
	if err := preparePackageArchives(context.Background(), connected,
		compileOptions{projectRoot: t.TempDir(), resourceCache: resourcecache.Settings{Directory: directory}},
		protocol.Metadata{Capabilities: protocol.Capabilities{ResourceArchiveUploads: true}}, environment); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("ready environment created a local cache: %v", err)
	}
}

func TestResourcePreparationReusesGlobalArchiveAcrossProjectsAndServerEviction(t *testing.T) {
	environment, data := resourceArchiveCommandFixture(t)
	cacheDirectory := filepath.Join(t.TempDir(), "global-cache")
	var downloads atomic.Int32
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		if r.URL.Path != "/archive/algorithms.r42.tar.xz" || r.Header.Get("Authorization") != "" {
			t.Errorf("archive mirror received wrong request or compiler credentials")
		}
		_, _ = w.Write(data)
	}))
	policy := &resources.SourcePolicy{Version: 1, ArchiveMirrors: []string{mirror.URL}}
	cache, err := resourcecache.Open(resourcecache.Settings{Directory: cacheDirectory})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := cache.Acquire(
		context.Background(),
		resources.ArchiveFetchOptions{
			Client:   mirror.Client(),
			Package:  environment.Packages[0],
			Policy:   policy,
			MaxBytes: 1024,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	// Subsequent CLI preparation must succeed with this mirror unavailable.
	mirror.Close()
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer compiler-token" {
			t.Error("compiler request omitted authentication")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/resolve":
			_ = json.NewEncoder(w).Encode(environment)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/"+environment.Digest+"/package-archives":
			// Simulate a cold/evicted remote archive on every invocation.
			_ = json.NewEncoder(
				w,
			).Encode(
				resources.PackageArchives{EnvironmentDigest: environment.Digest, Sources: policy,
					Archives: []resources.PackageArchive{{Package: environment.Packages[0], MaxBytes: 1024}}},
			)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/environments/"+environment.Digest+"/package-archives/"+environment.Packages[0].Checksum:
			got, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(got, data) || r.ContentLength != int64(len(data)) {
				t.Errorf(
					"archive upload differs from verified cache: length=%d bytes=%q err=%v",
					r.ContentLength,
					got,
					err,
				)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/"+environment.Digest+"/prepare":
			ready := environment
			ready.Ready = true
			_ = json.NewEncoder(w).Encode(ready)
		default:
			t.Errorf("unexpected preparation request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	connected, _ := client.New(server.URL, "compiler-token", 0, false)
	meta := protocol.Metadata{
		Capabilities: protocol.Capabilities{ResourceEnvironments: true, ResourceArchiveUploads: true},
	}
	for range 2 {
		project := t.TempDir()
		opts := compileOptions{
			projectRoot:   project,
			engine:        "xelatex",
			resourceCache: resourcecache.Settings{Directory: cacheDirectory},
		}
		ready, err := resolveResourceEnvironment(context.Background(), connected, opts, meta, nil, "default",
			resources.ResolveRequest{Resources: environment.Resources}, resourcePrepare)
		if err != nil || !ready.Ready {
			t.Fatalf("resource preparation failed: ready=%v error=%v", ready.Ready, err)
		}
		want := []string{
			"POST /v1/environments/resolve",
			"GET /v1/environments/" + environment.Digest + "/package-archives",
			"PUT /v1/environments/" + environment.Digest + "/package-archives/" + environment.Packages[0].Checksum,
			"POST /v1/environments/" + environment.Digest + "/prepare",
		}
		mu.Lock()
		got := append([]string{}, calls...)
		calls = nil
		mu.Unlock()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("preparation ordering: got%q want%q", got, want)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("global cache redownloaded across projects/remote eviction: %d", downloads.Load())
	}
}
