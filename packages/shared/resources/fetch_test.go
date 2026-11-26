package resources

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func archiveFixture() ([]byte, Package) {
	data := append(
		[]byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00},
		[]byte("archive bytes verified before any publication")...)
	hash := sha512.Sum512(data)
	return data, Package{Name: "algorithmicx", Revision: 15878, Checksum: hex.EncodeToString(hash[:])}
}

type archiveRoundTrip func(*http.Request) (*http.Response, error)

func (f archiveRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestFetchArchiveTransportClassificationAndCause(t *testing.T) {
	_, pkg := archiveFixture()
	for _, test := range []struct {
		name, code, reason string
		cause              error
		retryable          bool
	}{
		{"DNS missing", "resource_upstream_network", "dns", &net.DNSError{IsNotFound: true}, false},
		{"DNS timeout", "resource_upstream_network", "dns", &net.DNSError{IsTimeout: true}, true},
		{"TLS", "resource_upstream_tls", "verification", x509.UnknownAuthorityError{}, false},
		{"read interrupted", "resource_upstream_network", "network", io.ErrUnexpectedEOF, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{
				Transport: archiveRoundTrip(func(*http.Request) (*http.Response, error) { return nil, test.cause }),
			}
			_, err := FetchArchive(context.Background(), ArchiveFetchOptions{
				Client: client, Repository: "https://snapshot.example", Package: pkg, MaxBytes: 4096,
			}, func(write func(io.Writer) error) error { return write(io.Discard) })
			var failure *AcquisitionError
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Reason != test.reason ||
				failure.Retryable != test.retryable || !errors.Is(err, test.cause) {
				t.Fatalf("transport failure lost classification/cause: %#v %v", failure, err)
			}
		})
	}
}

func TestFetchArchiveRejectsHTTPSDowngradeWithoutAnotherAttempt(t *testing.T) {
	_, pkg := archiveFixture()
	var calls int
	client := &http.Client{Transport: archiveRoundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://insecure.example/archive"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	_, err := FetchArchive(context.Background(), ArchiveFetchOptions{Client: client, Package: pkg, MaxBytes: 4096,
		Policy: &SourcePolicy{Version: 1, ArchiveMirrors: []string{"https://first.example", "https://second.example"}},
	}, func(write func(io.Writer) error) error { return write(io.Discard) })
	var failure *AcquisitionError
	if !errors.As(err, &failure) || failure.Reason != "redirect-policy" || failure.Retryable ||
		failure.FallbackAllowed ||
		calls != 1 {
		t.Fatalf("redirect policy failed open: %#v calls=%d %v", failure, calls, err)
	}
}

func TestFetchArchiveClassifiesFailuresWithoutPublishing(t *testing.T) {
	data, pkg := archiveFixture()
	for _, test := range []struct {
		name, code, reason string
		status             int
		body               []byte
		limit              int64
	}{
		{"HTML200", "resource_upstream_format", "html", 200, []byte("<!DOCTYPE html><html>challenge</html>"), 4096},
		{"invalid XZ", "resource_upstream_format", "xz-magic", 200, []byte("not an xz archive"), 4096},
		{"checksum", "resource_archive_checksum", "sha512", 200, append(bytes.Clone(data), 'x'), 4096},
		{"size", "resource_limit_exceeded", "download-bytes", 200, data, 8},
		{"upstream403", "resource_upstream_http", "http-status", 403, []byte("private"), 4096},
		{"upstream429", "resource_upstream_http", "http-status", 429, nil, 4096},
		{"upstream503", "resource_upstream_http", "http-status", 503, nil, 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "00030")
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			directory := t.TempDir()
			root, err := safefs.Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			_, err = FetchArchive(context.Background(), ArchiveFetchOptions{
				Client: server.Client(), Repository: server.URL, Package: pkg, MaxBytes: test.limit,
			}, func(write func(io.Writer) error) error {
				pending, err := root.Stage(pkg.Checksum+".tar.xz", test.limit, write)
				if err != nil {
					return err
				}
				defer func() { _ = pending.Close() }()
				return pending.Commit()
			})
			var failure *AcquisitionError
			if !errors.As(err, &failure) || failure.Code != test.code || failure.Reason != test.reason ||
				len(failure.Attempts) != 1 || failure.UpstreamStatus != test.status {
				t.Fatalf("wrong failure: %#v %v", failure, err)
			}
			wantRetry := ""
			if test.status == http.StatusTooManyRequests || test.status == http.StatusServiceUnavailable {
				wantRetry = "30"
			}
			if failure.RetryAfter != wantRetry || failure.Attempts[0].RetryAfter != wantRetry {
				t.Fatalf("Retry-After not safely conveyed: %#v", failure)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed acquisition left cache bytes: %v %v", entries, err)
			}
			if strings.Contains(failure.Error(), "challenge") || strings.Contains(failure.Error(), server.URL) {
				t.Fatal("failure leaked response content or source URL")
			}
		})
	}
}

func TestFetchArchiveRevisionFallbackUsesFreshStagingAndKeepsIdentity(t *testing.T) {
	data, pkg := archiveFixture()
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/octet-stream")
		if strings.HasPrefix(r.URL.Path, "/bad/") {
			_, _ = w.Write(append(bytes.Clone(data), '!'))
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	policy := &SourcePolicy{Version: 1, ArchiveMirrors: []string{server.URL + "/bad", server.URL + "/good"}}
	directory := t.TempDir()
	root, err := safefs.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	name := pkg.Checksum + ".tar.xz"
	result, err := FetchArchive(context.Background(), ArchiveFetchOptions{
		Client:     server.Client(),
		Repository: "https://unused.invalid/snapshot",
		Package:    pkg,
		Policy:     policy,
		MaxBytes:   4096,
	}, func(write func(io.Writer) error) error {
		pending, err := root.Stage(name, 4096, write)
		if err != nil {
			return err
		}
		defer func() { _ = pending.Close() }()
		return pending.Commit()
	})
	if err != nil || result.Bytes != int64(len(data)) || result.SourceID != "mirror-2" || len(result.Attempts) != 2 ||
		result.Attempts[0].Code != "resource_archive_checksum" {
		t.Fatalf("fallback failed: %#v %v", result, err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("published bytes include a failed attempt: %q %v", actual, err)
	}
	if strings.Join(paths, ",") != "/bad/archive/algorithmicx.r15878.tar.xz,/good/archive/algorithmicx.r15878.tar.xz" {
		t.Fatalf("wrong revision archive paths: %v", paths)
	}
}

type failingArchiveWriter struct{ err error }

func (w failingArchiveWriter) Write([]byte) (int, error) { return 0, w.err }

func TestFetchArchiveLocalWriteFailureNeverFallsBack(t *testing.T) {
	data, pkg := archiveFixture()
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	cause := errors.New("private local disk error with a secret path")
	_, err := FetchArchive(context.Background(), ArchiveFetchOptions{
		Client: server.Client(), Package: pkg, MaxBytes: 4096,
		Policy: &SourcePolicy{Version: 1, ArchiveMirrors: []string{server.URL + "/one", server.URL + "/two"}},
	}, func(write func(io.Writer) error) error { return write(failingArchiveWriter{cause}) })
	var failure *AcquisitionError
	if !errors.As(err, &failure) || failure.Code != "resource_storage_failed" || failure.FallbackAllowed ||
		hits.Load() != 1 ||
		!errors.Is(err, cause) ||
		strings.Contains(err.Error(), "secret") {
		t.Fatalf("local error misclassified: %#v %v", failure, err)
	}
}

func TestFetchArchiveSourceTimeoutFallsBackButOverallCancellationStops(t *testing.T) {
	data, pkg := archiveFixture()
	var good atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/slow/") {
			<-r.Context().Done()
			return
		}
		good.Add(1)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	options := ArchiveFetchOptions{
		Client:   server.Client(),
		Package:  pkg,
		MaxBytes: 4096,
		Policy: &SourcePolicy{
			Version:          1,
			ArchiveMirrors:   []string{server.URL + "/slow", server.URL + "/good"},
			PerSourceTimeout: "100ms",
		},
	}
	publish := func(write func(io.Writer) error) error { return write(io.Discard) }
	result, err := FetchArchive(context.Background(), options, publish)
	if err != nil || result.SourceID != "mirror-2" || good.Load() != 1 {
		t.Fatalf("per-source deadline prevented fallback: %#v %v", result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err = FetchArchive(ctx, options, publish)
	var failure *AcquisitionError
	if !errors.As(err, &failure) || failure.Code != "resource_prepare_timeout" ||
		!errors.Is(err, context.DeadlineExceeded) ||
		good.Load() != 1 {
		t.Fatalf("overall deadline permitted another source: %#v %v", failure, err)
	}
}
