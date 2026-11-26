package httpapi

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/billstark001/latexmk/packages/server/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestPackageArchiveRoutesUseResolvedIdentityAndPreserveEnvironmentSchema(t *testing.T) {
	s := newTestServer(t, false)
	cfg := s.cfg
	cfg.ResourceInventoryDir = t.TempDir()
	cfg.MaxResourceBytes = 1024
	cfg.ResourceSourcePolicy = &model.SourcePolicy{Version: 1, ArchiveMirrors: []string{"https://mirror.example/tlnet"}}
	const repository = "https://snapshot.example/2026/09/09/tlnet"
	if err := os.WriteFile(
		filepath.Join(cfg.ResourceInventoryDir, "texlive-repository.txt"),
		[]byte(repository),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	manager, err := resources.New(cfg, protocol.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	s.environments, s.cfg = manager, cfg
	baseline := manager.List()[0]
	content := []byte("exact locked archive")
	sum := sha512.Sum512(content)
	checksum := hex.EncodeToString(sum[:])
	key := sha256.Sum256([]byte(baseline.Digest + "\x00" + repository))
	metadata := fmt.Sprintf(
		"name algorithms\ncategory Package\nrevision 42\nrelocated 1\ncontainerchecksum %s\nrunfiles size=1\n RELOC/tex/latex/algorithms/algorithm.sty\n",
		checksum,
	)
	if err := os.WriteFile(
		filepath.Join(cfg.StateDir, "environments", "metadata", hex.EncodeToString(key[:])+".tlpdb"),
		[]byte(metadata),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, content []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, bytes.NewReader(content))
		request.Header.Set("X-Request-ID", "req_archive_contract")
		s.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	request, _ := json.Marshal(
		model.ResolveRequest{
			Resources: []model.Resource{{ID: "algorithms", Type: "texlive", Packages: []string{"algorithms"}}},
		},
	)
	resolved := call(http.MethodPost, "/v1/environments/resolve", request)
	var environment model.Environment
	if resolved.Code != http.StatusOK || json.Unmarshal(resolved.Body.Bytes(), &environment) != nil ||
		len(environment.Packages) != 1 {
		t.Fatalf("resolve: %d %s", resolved.Code, resolved.Body.String())
	}
	base := "/v1/environments/" + environment.Digest
	list := call(http.MethodGet, base+"/package-archives", nil)
	var manifest model.PackageArchives
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &manifest) != nil ||
		len(manifest.Archives) != 1 ||
		manifest.Archives[0].Present ||
		manifest.Sources == nil {
		t.Fatalf("archive manifest: %d %s", list.Code, list.Body.String())
	}
	if manifest.EnvironmentDigest != environment.Digest || manifest.Archives[0].Package != environment.Packages[0] ||
		manifest.Archives[0].MaxBytes != 1024 {
		t.Fatalf("manifest changed resolved identity: %+v", manifest)
	}
	if got := call(
		http.MethodPut,
		base+"/package-archives/"+strings.Repeat("a", 128),
		content,
	); got.Code != http.StatusNotFound {
		t.Fatalf("accepted undeclared package archive: %d %s", got.Code, got.Body.String())
	}
	if got := call(
		http.MethodPut,
		base+"/package-archives/"+checksum,
		[]byte("wrong"),
	); got.Code != http.StatusBadRequest ||
		!strings.Contains(got.Body.String(), `"code":"resource_archive_checksum"`) {
		t.Fatalf("wrong archive body: %d %s", got.Code, got.Body.String())
	}
	for range 2 {
		if got := call(
			http.MethodPut,
			base+"/package-archives/"+checksum,
			content,
		); got.Code != http.StatusNoContent ||
			got.Header().Get("X-Request-ID") != "req_archive_contract" {
			t.Fatalf("idempotent archive upload: %d %s", got.Code, got.Body.String())
		}
	}
	list = call(http.MethodGet, base+"/package-archives", nil)
	if json.Unmarshal(list.Body.Bytes(), &manifest) != nil || !manifest.Archives[0].Present {
		t.Fatalf("archive not present: %s", list.Body.String())
	}
	inspected := call(http.MethodGet, base, nil)
	var fields map[string]json.RawMessage
	if inspected.Code != http.StatusOK || json.Unmarshal(inspected.Body.Bytes(), &fields) != nil {
		t.Fatal(inspected.Body.String())
	}
	if _, exists := fields["sources"]; exists {
		t.Fatal("transport policy entered existing environment schema")
	}
	if _, exists := fields["archives"]; exists {
		t.Fatal("archive availability entered existing environment schema")
	}
	if got := call(
		http.MethodGet,
		"/v1/environments/"+strings.Repeat("f", 64)+"/package-archives",
		nil,
	); got.Code != http.StatusNotFound {
		t.Fatalf("unknown environment exposed: %d", got.Code)
	}
	// A stalled client body must not outlive the preparation timeout. A context
	// check between reads alone would leave this request blocked indefinitely.
	s.cfg.ResourcePrepareTimeout = 30 * time.Millisecond
	live := httptest.NewServer(s.Handler())
	defer live.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(live.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(
		connection,
		"PUT %s/package-archives/%s HTTP/1.1\r\nHost: compiler\r\nContent-Length: %d\r\n\r\nx",
		base,
		checksum,
		len(content),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPut})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("stalled upload status = %d", response.StatusCode)
	}
}

func TestTypedResourceFailureKeepsLegacyErrorAndBoundsSafeDetails(t *testing.T) {
	s := newTestServer(t, false)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/environments/digest/prepare", nil)
	failure := &model.AcquisitionError{
		Code: "resource_archive_unavailable", Stage: "archive-download", Reason: strings.Repeat("x", 500),
		Retryable: true, FallbackAllowed: true, Cause: errors.New("secret-token <html>private body</html>"),
		Attempts: make([]model.AcquisitionAttempt, 20),
	}
	for i := range failure.Attempts {
		failure.Attempts[i].ContentType = strings.Repeat("x", 400)
	}
	s.resourceError(c, failure)
	var body struct {
		Error     string                 `json:"error"`
		Code      string                 `json:"code"`
		Retryable bool                   `json:"retryable"`
		Details   model.AcquisitionError `json:"details"`
	}
	if recorder.Code != http.StatusBadGateway || json.Unmarshal(recorder.Body.Bytes(), &body) != nil ||
		body.Error == "" ||
		!body.Retryable ||
		!body.Details.FallbackAllowed {
		t.Fatalf("typed resource failure: %d %s", recorder.Code, recorder.Body.String())
	}
	if len(body.Details.Attempts) != 8 || len(body.Details.Reason) != 128 ||
		len(body.Details.Attempts[0].ContentType) != 128 ||
		strings.Contains(recorder.Body.String(), "secret-token") ||
		strings.Contains(recorder.Body.String(), "<html>") {
		t.Fatalf("unbounded or private failure details: %s", recorder.Body.String())
	}
	if failure.Cause == nil || len(failure.Attempts) != 20 || len(failure.Attempts[0].ContentType) != 400 {
		t.Fatal("HTTP formatting mutated the original failure")
	}
	for _, test := range []struct {
		name        string
		err         error
		status      int
		code, stage string
	}{
		{"configured quota", resources.ErrQuota, http.StatusRequestEntityTooLarge, "resource_quota_exceeded", "resource-prepare"},
		{"temporary capacity", &model.AcquisitionError{Code: "resource_cache_unavailable", Stage: "resource-cache", Retryable: true}, http.StatusServiceUnavailable, "resource_cache_unavailable", "resource-cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/environments/digest/prepare", nil)
			s.resourceError(c, test.err)
			if recorder.Code != test.status || json.Unmarshal(recorder.Body.Bytes(), &body) != nil ||
				body.Code != test.code ||
				body.Details.Stage != test.stage {
				t.Fatalf("quota classification: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestUpstreamRetryAfterIsCanonicalAndOnlyForwardedForFinalBackpressure(t *testing.T) {
	s := newTestServer(t, false)
	for _, test := range []struct {
		upstream int
		input    string
		want     string
	}{
		{http.StatusTooManyRequests, "00030", "30"},
		{http.StatusServiceUnavailable, "Wed, 21 Oct 2015 07:28:00 GMT", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{http.StatusServiceUnavailable, "30\r\nX-Secret: value", ""},
		{http.StatusForbidden, "30", ""},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/environments/digest/prepare", nil)
		s.resourceError(c, &model.AcquisitionError{Code: "resource_upstream_http", Stage: "archive-fetch",
			UpstreamStatus: test.upstream, RetryAfter: test.input, Retryable: true})
		if got := recorder.Header().Get("Retry-After"); got != test.want {
			t.Fatalf("upstream %d Retry-After %q forwarded as %q, want %q", test.upstream, test.input, got, test.want)
		}
		if strings.Contains(recorder.Body.String(), "X-Secret") {
			t.Fatal("invalid upstream retry metadata entered response details")
		}
	}
}
