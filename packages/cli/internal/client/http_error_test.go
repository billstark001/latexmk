package client

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestHTTPFailuresPreserveSafeContextAcrossCompileTransports(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "req_service_failure")
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"queue unavailable"}`)
	}))
	defer server.Close()
	c, err := New(server.URL, "private-token", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot, c.ProjectID = t.TempDir(), "project_test"
	file := filepath.Join(c.ProjectRoot, "source.tex")
	if err := os.WriteFile(file, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	source := projectarchive.File{Path: "source.tex", Source: file}
	request := protocol.CompileRequest{Entry: "source.tex", Engine: "xelatex"}
	for name, invoke := range map[string]func() error{
		"metadata":    func() error { _, err := c.Metadata(context.Background()); return err },
		"health":      func() error { return c.Health(context.Background()) },
		"upload-plan": func() error { _, err := c.uploadManifest(context.Background(), request, nil); return err },
		"upload-blob": func() error { return c.uploadBlob(context.Background(), "upload_test", "digest", source) },
		"poll-job":    func() error { _, err := c.GetJob(context.Background(), "job_test"); return err },
		"result": func() error {
			response, err := c.resultResponse(context.Background(), "job_test")
			if err == nil {
				_ = response.Body.Close()
			}
			return err
		},
		"revision": func() error {
			_, err := c.CommitRevision(context.Background(), PreparedRevision{SessionID: "ses_test"})
			return err
		},
		"legacy": func() error { _, err := c.compileLegacy(context.Background(), request, t.TempDir(), nil); return err },
		"SSE": func() error {
			return c.StreamSessionEvents(context.Background(), "ses_test", 0, func(protocol.SessionEvent) {})
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := invoke()
			var failure *HTTPError
			if !errors.As(err, &failure) || failure.StatusCode != 503 ||
				failure.Message != "queue unavailable" || failure.RequestID != "req_service_failure" || failure.RetryAfter != "30" ||
				failure.Method == "" || failure.Path == "" {
				t.Fatalf("lost HTTP context: %#v %v", failure, err)
			}
			if strings.Contains(err.Error(), c.Token) {
				t.Fatal("HTTP failure exposed the request credential")
			}
		})
	}
}

func TestHTTPErrorBodyIsBoundedAndNeverFallsBackToRawText(t *testing.T) {
	c := &Client{Token: "secret-token"}
	for _, test := range []struct{ name, body, issue string }{
		{"HTML", `<html><body>secret-token upstream detail</body></html>`, "non_json_error_body"},
		{"text", "secret-token proxy details", "non_json_error_body"},
		{"malformed", `{"error":"secret-token`, "non_json_error_body"},
		{"unknown-shape", `{"message":"secret-token"}`, "invalid_error_body"},
		{"nested-shape", `{"error":{"message":"secret-token"}}`, "invalid_error_body"},
		{"trailing-data", `{"error":"secret-token"} {}`, "non_json_error_body"},
		{"oversized", `{"error":"secret-token"}` + strings.Repeat(" ", maxErrorBodyBytes), "error_body_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &errorCountingReader{Reader: strings.NewReader(test.body)}
			failure := c.readHTTPError(
				&http.Response{
					StatusCode: 502,
					Status:     "502 secret-token",
					Header:     http.Header{},
					Body:       io.NopCloser(reader),
				},
			)
			if failure.ResponseIssue != test.issue || failure.Message != "" ||
				strings.Contains(failure.Error(), "secret-token") ||
				strings.Contains(failure.Error(), "<html>") ||
				reader.bytes > maxErrorBodyBytes+1 {
				t.Fatalf("unsafe error: %#v read=%d", failure, reader.bytes)
			}
			if failure.BodyTruncated != (test.name == "oversized") {
				t.Fatalf("wrong truncation indicator: %#v", failure)
			}
		})
	}
	failure := c.readHTTPError(
		&http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`{"error":"Bearer secret-token rejected\u001b[31m\n` + strings.Repeat("字", maxErrorMessageRunes) + `"}`,
		))},
	)
	if strings.Contains(failure.Message, c.Token) || strings.ContainsAny(failure.Message, "\x1b\n") ||
		!failure.MessageTruncated || utf8.RuneCountInString(failure.Message) != maxErrorMessageRunes {
		t.Fatalf("unbounded or unsafe message: %#v", failure)
	}
	if !strings.Contains(failure.Message, "[REDACTED]") {
		t.Fatal("credential was not redacted")
	}
	failure = c.readHTTPError(
		&http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(errorBodyReader{})},
	)
	if failure.ResponseIssue != "error_body_read_failed" || failure.Message != "" ||
		!errors.Is(failure, io.ErrUnexpectedEOF) {
		t.Fatalf("body read failure was ignored: %#v", failure)
	}
}

type errorCountingReader struct {
	io.Reader
	bytes int
}

func (r *errorCountingReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.bytes += n
	return n, err
}

type errorBodyReader struct{}

func (errorBodyReader) Read(buffer []byte) (int, error) {
	return copy(buffer, `{"error":"partial secret-token"}`), io.ErrUnexpectedEOF
}

func TestHTTPErrorHeaderAllowlistAndRequestTarget(t *testing.T) {
	c := &Client{Token: "secret-token"}
	request, err := http.NewRequest(
		http.MethodPost,
		"https://compiler.example/v1/jobs/secret-token?credential=private-query",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{StatusCode: 429, Header: http.Header{
		"X-Request-Id": {
			"req_rate_limited",
		},
		"Retry-After":   {"00030"},
		"Content-Type":  {"application/json; charset=utf-8"},
		"Set-Cookie":    {"session=secret-token"},
		"Authorization": {"Bearer secret-token"},
	}, Request: request}
	failure := c.responseError(response, "", nil)
	if failure.RequestID != "req_rate_limited" || failure.RetryAfter != "30" ||
		failure.ContentType != "application/json" ||
		strings.Contains(failure.Error(), "private-query") ||
		strings.Contains(failure.Error(), "secret-token") {
		t.Fatalf("unsafe headers or target: %#v", failure)
	}
	for _, value := range []string{"not a delay", "-1", "+1", "1\nheader", strings.Repeat("9", 200)} {
		response.Header.Set("Retry-After", value)
		response.Header.Set("X-Request-ID", "invalid request id")
		failure = c.responseError(response, "", nil)
		if failure.RetryAfter != "" || failure.RequestID != "" {
			t.Fatalf("accepted unsafe header %q: %#v", value, failure)
		}
	}
	date := "Mon, 23 Nov 2026 00:00:00 GMT"
	response.Header.Set("Retry-After", date)
	if got := c.responseError(response, "", nil).RetryAfter; got != date {
		t.Fatalf("HTTP-date retry-after = %q", got)
	}
}

func TestSuccessfulResponseFailuresKeepHTTPContext(t *testing.T) {
	for _, body := range []string{`<html>secret-token</html>`, `null`, `{} {}`, `{"secret-token":true}`} {
		t.Run(body[:min(len(body), 16)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req_invalid_json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			c, err := New(server.URL, "secret-token", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.GetJob(context.Background(), "job_test")
			var failure *HTTPError
			if !errors.As(err, &failure) || failure.StatusCode != 200 || failure.RequestID != "req_invalid_json" ||
				failure.ResponseIssue != "invalid_api_response" || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("invalid response context: %#v %v", failure, err)
			}
		})
	}
}

func TestResultResponseErrorsDoNotReclassifyLocalWrites(t *testing.T) {
	pdf := []byte("pdf")
	digest := sha256.Sum256(pdf)
	archive := buildResultArchive(t, []tarEntry{
		{
			name: "result.json",
			payload: []byte(
				`{"protocolVersion":2,"success":true,"artifacts":[{"path":"main.pdf","size":3,"sha256":"` + hex.EncodeToString(
					digest[:],
				) + `"}]}`,
			),
		},
		{name: "artifacts/main.pdf", payload: pdf},
	})
	for _, invalidRemote := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalidRemote), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req_result")
				if invalidRemote {
					_, _ = io.WriteString(w, "<html>not a result</html>")
					return
				}
				_, _ = w.Write(archive)
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			c.ProjectRoot = t.TempDir()
			output := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(output, nil, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = c.compileLegacy(context.Background(), protocol.CompileRequest{Entry: "main.tex"}, output, nil)
			var failure *HTTPError
			if invalidRemote {
				if !errors.As(err, &failure) || failure.StatusCode != 200 ||
					failure.ResponseIssue != "invalid_result_archive" {
					t.Fatalf("lost result HTTP context: %v", err)
				}
			} else if err == nil || errors.As(err, &failure) {
				t.Fatalf("local write became an HTTP error: %v", err)
			}
		})
	}
}

func TestSSEFailuresKeepStatusAndCleanEOFStaysEOF(t *testing.T) {
	for name, body := range map[string]string{"invalid-type": "<html>proxy</html>", "invalid-event": "data: not JSON\n\n", "clean-EOF": ""} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req_events")
				if name != "invalid-type" {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			err = c.StreamSessionEvents(context.Background(), "ses_test", 0, func(protocol.SessionEvent) {})
			var failure *HTTPError
			if name == "clean-EOF" {
				if !errors.Is(err, io.EOF) || errors.As(err, &failure) {
					t.Fatalf("clean EOF became invalid response: %v", err)
				}
			} else if !errors.As(err, &failure) || failure.StatusCode != 200 || failure.RequestID != "req_events" {
				t.Fatalf("missing SSE response context: %v", err)
			}
		})
	}
}

func TestInvalidServerURLDoesNotEchoCredentials(t *testing.T) {
	for _, endpoint := range []string{"http://user:private-password@%zz", "http://compiler.invalid?token=private-token%zz"} {
		_, err := New(endpoint, "", time.Second, false)
		if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), endpoint) {
			t.Fatalf("invalid URL exposed credentials: %v", err)
		}
	}
}

func TestTransportFailuresPreserveCausesWithoutEchoingProxyResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = buffer.WriteString("untrusted-proxy-line private-token\r\n\r\n")
		if err := buffer.Flush(); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "private-token", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetJob(context.Background(), "job_test")
	var transport *requestFailure
	var response *HTTPError
	if !errors.As(err, &transport) || errors.As(err, &response) || strings.Contains(err.Error(), "private-token") ||
		strings.Contains(err.Error(), "untrusted-proxy-line") {
		t.Fatalf("proxy body was exposed or given an HTTP status: %v", err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://compiler.example/v1/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		cause error
		want  string
	}{
		{context.DeadlineExceeded, "deadline"}, {context.Canceled, "canceled"},
		{&net.DNSError{Err: "untrusted-proxy-line", Name: "private-token"}, "DNS"},
		{x509.UnknownAuthorityError{}, "TLS certificate"},
		{syscall.ECONNREFUSED, "refused"}, {syscall.ECONNRESET, "reset"},
		{io.ErrUnexpectedEOF, "complete HTTP response"},
	} {
		failure := c.failedRequest(request, test.cause)
		if !errors.Is(failure, test.cause) || !strings.Contains(failure.Error(), test.want) ||
			strings.Contains(
				failure.Error(),
				"private-token",
			) || strings.Contains(failure.Error(), "untrusted-proxy-line") {
			t.Fatalf("unsafe or incomplete transport failure: %v", failure)
		}
	}
}

func TestResultIntegrityFailuresRetainReceivedHTTPContext(t *testing.T) {
	for _, name := range []string{"omitted-artifact", "artifact-size", "artifact-checksum", "compile-checksum", "log-checksum", "diagnostic-checksum", "live-identity"} {
		t.Run(name, func(t *testing.T) {
			path, expected, received := "main.pdf", "pdf", "bad"
			if name == "log-checksum" || name == "diagnostic-checksum" {
				path, expected = "main.log", "log"
			}
			hash := sha256.Sum256([]byte(expected))
			result := protocol.CompileResult{ProtocolVersion: protocol.Version, RequestID: "job_test", Success: true,
				Artifacts: []protocol.Artifact{{Path: path, Size: 3, SHA256: hex.EncodeToString(hash[:])}}}
			if name == "live-identity" {
				result.RequestID, result.SessionID, result.Revision, result.Artifacts = "other_job", "ses_test", 1, nil
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			entries := []tarEntry{{name: "result.json", payload: raw}}
			if name == "artifact-size" {
				received = "bad!"
			}
			if name != "omitted-artifact" && name != "live-identity" {
				entries = append(entries, tarEntry{name: "artifacts/" + path, payload: []byte(received)})
			}
			archive := buildResultArchive(t, entries)
			job := protocol.Job{
				ID:        "job_test",
				Status:    "succeeded",
				SessionID: "ses_test",
				Revision:  1,
				Result:    &result,
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req_integrity")
				if r.URL.Path == "/v1/jobs/job_test" {
					if err := json.NewEncoder(w).Encode(job); err != nil {
						t.Error(err)
					}
					return
				}
				_, _ = w.Write(archive)
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			c.ProjectRoot = t.TempDir()
			switch name {
			case "compile-checksum":
				_, err = c.compileLegacy(
					context.Background(),
					protocol.CompileRequest{Entry: "main.tex"},
					t.TempDir(),
					nil,
				)
			case "log-checksum":
				_, err = c.Logs(context.Background(), job.ID, "compiler", 10, 1024)
			case "diagnostic-checksum":
				_, err = c.Diagnostics(context.Background(), job.ID)
			case "live-identity":
				_, _, err = c.DownloadLiveResult(
					context.Background(),
					job,
					protocol.CompileRequest{Entry: "main.tex"},
					t.TempDir(),
				)
			default:
				_, err = c.DownloadArtifact(context.Background(), job.ID, artifactID(path), t.TempDir())
			}
			var failure *HTTPError
			if !errors.As(err, &failure) || failure.StatusCode != 200 || failure.RequestID != "req_integrity" ||
				failure.ResponseIssue != "invalid_result_archive" {
				t.Fatalf("remote integrity error lost HTTP context: %v", err)
			}
		})
	}
	// A destination can fail with a non-PathError; error origin, not its text or
	// concrete filesystem type, determines whether HTTP context can be attached.
	hash := sha256.Sum256([]byte("pdf"))
	err := verifyArtifactInput(failingArtifactOutput{}, strings.NewReader("pdf"), 3, hex.EncodeToString(hash[:]))
	var remote *responseDecodeError
	if !errors.Is(err, io.ErrClosedPipe) || errors.As(err, &remote) {
		t.Fatalf("local writer failure marked as remote input: %v", err)
	}
}

type failingArtifactOutput struct{}

func (failingArtifactOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
