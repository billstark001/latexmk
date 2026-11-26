package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestCompileHTTPFailureJSONDuringPreparation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if err := os.WriteFile(
		"main.tex",
		[]byte(`\documentclass{article}\begin{document}x\end{document}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req_compile_failure")
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"deployment unavailable; Bearer private-token"}`)
	}))
	defer server.Close()
	for _, detached := range []bool{false, true} {
		t.Run(strconv.FormatBool(detached), func(t *testing.T) {
			args := []string{
				"latexmk",
				"--json",
				"--server",
				server.URL,
				"--token",
				"private-token",
				"--project-id",
				"project_test",
				"main.tex",
			}
			command := "compile"
			if detached {
				args = append(args, "--detach")
				command = "compile.start"
			}
			code, stdout, stderr := captureCommandOutput(t, func() int { return run(args) })
			var envelope agentJSONEnvelope
			if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
				t.Fatalf("non-JSON compile failure: %d %q %q: %v", code, stdout, stderr, err)
			}
			if code != 1 || envelope.OK || envelope.Command != command || envelope.Error == nil ||
				envelope.Error.Code != "server_error" || !envelope.Error.Retryable ||
				envelope.Error.Details["httpStatus"] != float64(
					503,
				) || envelope.Error.Details["requestId"] != "req_compile_failure" ||
				envelope.Error.Details["retryAfter"] != "15" || strings.Contains(stdout+stderr, "private-token") {
				t.Fatalf("incomplete or unsafe error: code=%d %s %s", code, stdout, stderr)
			}
		})
	}
}

func TestHTTPClassificationKeepsStatusAndRetryContract(t *testing.T) {
	for _, test := range []struct {
		status    int
		code      string
		retryable bool
	}{
		{400, "http_error", false}, {401, "authentication_failed", false}, {403, "authentication_failed", false},
		{404, "not_found", false}, {408, "http_error", true}, {409, "conflict", false}, {413, "http_error", false},
		{429, "rate_limited", true}, {500, "server_error", true}, {502, "server_error", true}, {503, "server_error", true},
		{504, "server_error", true}, {200, "invalid_response", false},
	} {
		failure := &client.HTTPError{
			StatusCode: test.status,
			RequestID:  "req_failure",
			RetryAfter: "30",
			Method:     "POST",
			Path:       "/v1/uploads/plans",
		}
		code, details, retryable, exit := classifyAgentError(fmt.Errorf("submit: %w", failure))
		if code != test.code || retryable != test.retryable || exit != 1 || details["httpStatus"] != test.status ||
			details["requestId"] != "req_failure" || details["retryAfter"] != "30" {
			t.Fatalf("status %d: %s %v retryable=%t exit=%d", test.status, code, details, retryable, exit)
		}
	}
	// A received response can precede a body-read deadline or cancellation.
	for _, test := range []struct {
		cause error
		code  string
		exit  int
	}{
		{context.DeadlineExceeded, "timeout", 124}, {context.Canceled, "cancelled", 1},
		{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, "network_error", 1},
	} {
		err := errors.Join(&client.HTTPError{StatusCode: 200, RequestID: "req_body"}, test.cause)
		code, details, _, exit := classifyAgentError(err)
		if code != test.code || exit != test.exit || details["httpStatus"] != 200 ||
			details["requestId"] != "req_body" {
			t.Fatalf("body context lost: %s %v exit=%d", code, details, exit)
		}
	}
	for _, test := range []struct {
		status    int
		cause     error
		code      string
		retryable bool
	}{
		{401, context.DeadlineExceeded, "authentication_failed", false},
		{403, context.Canceled, "authentication_failed", false},
		{429, context.DeadlineExceeded, "rate_limited", true},
		{503, context.Canceled, "server_error", true},
	} {
		err := errors.Join(
			&client.HTTPError{StatusCode: test.status, ResponseIssue: "error_body_read_failed"},
			test.cause,
		)
		code, details, retryable, exit := classifyAgentError(err)
		if code != test.code || exit != 1 || retryable != test.retryable || details["httpStatus"] != test.status {
			t.Fatalf(
				"body-read error overrode received HTTP status: %s %v retryable=%t exit=%d",
				code,
				details,
				retryable,
				exit,
			)
		}
	}
	code, details, retryable, _ := classifyAgentError(
		&os.PathError{Op: "open", Path: "local-output", Err: os.ErrPermission},
	)
	if code != "command_failed" || details != nil || retryable {
		t.Fatalf("local path error was labelled as HTTP/network: %s %v %t", code, details, retryable)
	}
}

func TestTypedResourceRetryabilityOverridesHTTPDefault(t *testing.T) {
	for _, retry := range []bool{true, false} {
		failure := &client.HTTPError{StatusCode: 502, ServerCode: "resource_archive_checksum", Retryable: &retry,
			ResourceDetails: map[string]any{"stage": "archive-fetch", "package": "algorithmicx"}}
		code, details, retryable, exit := classifyAgentError(failure)
		if code != "server_error" || retryable != retry || exit != 1 ||
			details["serverCode"] != "resource_archive_checksum" {
			t.Fatalf("typed retry contract changed: %s %#v %t %d", code, details, retryable, exit)
		}
	}
}

func TestCompileReporterPreservesSuccessShapeAndHumanHTTPDetails(t *testing.T) {
	failure := &client.HTTPError{StatusCode: 429, RequestID: "req_capacity", RetryAfter: "60", Message: "queue full"}
	code, _, stderr := captureCommandOutput(t, func() int {
		return reportCompile(client.CompileOutput{}, failure, compileOptions{})
	})
	if code != 2 || !strings.Contains(stderr, "429 Too Many Requests") ||
		!strings.Contains(stderr, "request-id=req_capacity") ||
		!strings.Contains(stderr, "retry-after=60") ||
		!strings.Contains(stderr, "queue full") {
		t.Fatalf("incomplete human HTTP error: %d %q", code, stderr)
	}
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return reportCompile(
			client.CompileOutput{Result: protocol.CompileResult{Success: true, RequestID: "job_success"}},
			nil,
			compileOptions{jsonOutput: true},
		)
	})
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if code != 0 || stderr != "" || result["schemaVersion"] != nil || string(result["requestId"]) != `"job_success"` {
		t.Fatalf("compile success shape changed: %d %s %s", code, stdout, stderr)
	}
}
