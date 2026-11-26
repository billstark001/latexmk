package client

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/billstark001/latexmk/packages/shared/resources"
)

const maxErrorBodyBytes = 64 << 10
const maxErrorMessageRunes = 1024

// HTTPError describes a received response. Only allowlisted response metadata
// and a bounded JSON error message are retained; request credentials and raw
// response bodies are never included. A 2xx status with ResponseIssue indicates
// an invalid response, rather than a rejected request or a local filesystem error.
type HTTPError struct {
	StatusCode       int
	Message          string
	RequestID        string
	RetryAfter       string
	Method           string
	Path             string
	ContentType      string
	ResponseIssue    string
	BodyTruncated    bool
	MessageTruncated bool
	ServerCode       string
	Retryable        *bool
	ResourceDetails  map[string]any
	cause            error
}

func (e *HTTPError) Unwrap() error { return e.cause }

func (e *HTTPError) Error() string {
	message := "server returned " + responseStatus(e.StatusCode)
	if e.Method != "" {
		message += " for " + e.Method + " " + e.Path
	}
	var metadata []string
	if e.RequestID != "" {
		metadata = append(metadata, "request-id="+e.RequestID)
	}
	if e.RetryAfter != "" {
		metadata = append(metadata, "retry-after="+e.RetryAfter)
	}
	if len(metadata) > 0 {
		message += " [" + strings.Join(metadata, "; ") + "]"
	}
	if e.Message != "" {
		message += ": " + e.Message
	}
	if e.MessageTruncated {
		message += " [message truncated]"
	}
	if e.ResponseIssue != "" {
		message += "; " + responseIssueMessage(e.ResponseIssue)
		switch {
		case errors.Is(e.cause, context.DeadlineExceeded):
			message += " (response body deadline exceeded)"
		case errors.Is(e.cause, context.Canceled):
			message += " (response body read canceled)"
		case errors.Is(e.cause, syscall.ECONNRESET):
			message += " (connection reset while reading response body)"
		case errors.Is(e.cause, io.ErrUnexpectedEOF):
			message += " (response body ended unexpectedly)"
		}
		if e.ContentType != "" {
			message += " (content-type=" + e.ContentType + ")"
		}
	}
	return message
}

func responseStatus(status int) string {
	text := strconv.Itoa(status)
	if name := http.StatusText(status); name != "" {
		text += " " + name
	}
	return text
}

func responseIssueMessage(issue string) string {
	switch issue {
	case "non_json_error_body":
		return "non-JSON error body omitted"
	case "invalid_error_body":
		return "invalid JSON error body omitted"
	case "error_body_too_large":
		return "error body exceeded 64 KiB and was omitted"
	case "error_body_read_failed":
		return "error body could not be read completely and was omitted"
	case "invalid_api_response":
		return "invalid API JSON response"
	case "unexpected_event_content_type":
		return "expected a session event stream"
	case "invalid_event_stream":
		return "invalid session event stream"
	case "invalid_result_archive":
		return "invalid compiler result archive"
	default:
		return "invalid server response"
	}
}

func (c *Client) responseError(resp *http.Response, issue string, cause error) *HTTPError {
	failure := &HTTPError{
		StatusCode:    resp.StatusCode,
		ResponseIssue: issue,
		cause:         cause,
	}
	if mediaType, _, err := mime.ParseMediaType(
		resp.Header.Get("Content-Type"),
	); err == nil && len(mediaType) <= 128 &&
		!c.containsCredential(mediaType) {
		failure.ContentType = mediaType
	}
	if id := resp.Header.Get("X-Request-ID"); safeResponseID(id) && !c.containsCredential(id) {
		failure.RequestID = id
	}
	if retry := resp.Header.Get("Retry-After"); !c.containsCredential(retry) {
		failure.RetryAfter = resources.CanonicalRetryAfter(retry)
	}
	if resp.Request != nil {
		failure.Method = resp.Request.Method
		if resp.Request.URL != nil {
			failure.Path, _ = c.safeResponseText(resp.Request.URL.Path, 512)
		}
	}
	return failure
}

func (c *Client) readHTTPError(resp *http.Response) *HTTPError {
	failure := c.responseError(resp, "", nil)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	if err != nil {
		failure.ResponseIssue, failure.cause = "error_body_read_failed", err
		return failure
	}
	if len(body) > maxErrorBodyBytes {
		failure.ResponseIssue, failure.BodyTruncated = "error_body_too_large", true
		return failure
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return failure
	}
	if !json.Valid(body) {
		failure.ResponseIssue = "non_json_error_body"
		return failure
	}
	var payload struct {
		Error     string                     `json:"error"`
		Code      string                     `json:"code"`
		Retryable *bool                      `json:"retryable"`
		Details   map[string]json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Error == "" {
		failure.ResponseIssue = "invalid_error_body"
		return failure
	}
	failure.Message, failure.MessageTruncated = c.safeResponseText(payload.Error, maxErrorMessageRunes)
	if safeErrorCode(payload.Code) && !c.containsCredential(payload.Code) {
		failure.ServerCode = payload.Code
		failure.Retryable = payload.Retryable
		failure.ResourceDetails = c.resourceErrorDetails(payload.Details, true)
	}
	return failure
}

func safeErrorCode(code string) bool {
	if len(code) == 0 || len(code) > 80 {
		return false
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// Resource diagnostics are untrusted response data. Preserve a bounded, typed
// allowlist rather than forwarding arbitrary server fields or upstream bodies.
func (c *Client) resourceErrorDetails(fields map[string]json.RawMessage, attempts bool) map[string]any {
	result := make(map[string]any)
	for _, key := range []string{"stage", "reason", "package", "sourceId", "code", "contentType", "expected", "actual", "environmentDigest", "baselineDigest", "retryAfter"} {
		var value string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil {
			limit := 128
			if key == "reason" {
				limit = 256
			}
			if value, truncated := c.safeResponseText(
				value,
				limit,
			); value != "" && !truncated &&
				safeResourceField(key, value) {
				result[key] = value
			}
		}
	}
	for _, key := range []string{"revision", "upstreamStatus", "bytes", "limit"} {
		var value int64
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil && value >= 0 {
			if key != "upstreamStatus" || (value >= 100 && value <= 599) {
				result[key] = value
			}
		}
	}
	for _, key := range []string{"retryable", "fallbackAllowed"} {
		var value bool
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil {
			result[key] = value
		}
	}
	if attempts {
		var items []map[string]json.RawMessage
		if json.Unmarshal(fields["attempts"], &items) == nil && len(items) <= 8 {
			bounded := make([]map[string]any, 0, len(items))
			for _, item := range items {
				bounded = append(bounded, c.resourceErrorDetails(item, false))
			}
			if len(bounded) > 0 {
				result["attempts"] = bounded
			}
		}
	}
	return result
}

func safeResourceField(key, value string) bool {
	switch key {
	case "retryAfter":
		return resources.CanonicalRetryAfter(value) == value
	case "expected", "actual":
		if len(value) != 64 && len(value) != 128 {
			return false
		}
		_, err := hex.DecodeString(value)
		return err == nil
	case "environmentDigest", "baselineDigest":
		return resources.ValidDigest(value)
	case "contentType":
		mediaType, _, err := mime.ParseMediaType(value)
		return err == nil && value == mediaType
	default:
		for _, r := range value {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
				r != '_' && r != '-' && r != '.' && r != '+' {
				return false
			}
		}
		return true
	}
}

func (c *Client) containsCredential(value string) bool {
	return c.Token != "" && strings.Contains(value, c.Token)
}

func (c *Client) safeResponseText(value string, limit int) (string, bool) {
	if c.Token != "" {
		value = strings.ReplaceAll(value, c.Token, "[REDACTED]")
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]), true
	}
	return value, false
}

func safeResponseID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// responseDecodeError marks failures caused by remote result bytes. Local file
// creation/publication errors remain ordinary errors and must not gain HTTP status.
type responseDecodeError struct{ cause error }

func (e *responseDecodeError) Error() string { return e.cause.Error() }
func (e *responseDecodeError) Unwrap() error { return e.cause }

func (c *Client) resultResponseError(resp *http.Response, err error) error {
	var decoding *responseDecodeError
	if errors.As(err, &decoding) {
		return c.responseError(resp, "invalid_result_archive", err)
	}
	return err
}

func invalidResult(format string, args ...any) error {
	return &responseDecodeError{cause: fmt.Errorf(format, args...)}
}

// requestFailure has no HTTP status: no valid response was received. Keep the
// transport cause for error classification without printing URLs, credentials,
// malformed proxy status lines, or arbitrary underlying error text.
type requestFailure struct {
	message string
	cause   error
}

func (e *requestFailure) Error() string { return e.message }
func (e *requestFailure) Unwrap() error { return e.cause }

func (c *Client) failedRequest(request *http.Request, cause error) error {
	var dns *net.DNSError
	var network net.Error
	var authority x509.UnknownAuthorityError
	var certificate x509.CertificateInvalidError
	var hostname x509.HostnameError
	reason := "failed before a valid HTTP response"
	switch {
	case errors.Is(cause, context.Canceled):
		reason = "was canceled"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "exceeded its deadline"
	case errors.As(cause, &dns):
		reason = "failed DNS lookup"
	case errors.As(cause, &authority), errors.As(cause, &certificate), errors.As(cause, &hostname):
		reason = "failed TLS certificate validation"
	case errors.Is(cause, syscall.ECONNREFUSED):
		reason = "was refused by the remote endpoint"
	case errors.Is(cause, syscall.ECONNRESET):
		reason = "had its connection reset"
	case errors.As(cause, &network) && network.Timeout():
		reason = "timed out"
	case errors.Is(cause, io.EOF), errors.Is(cause, io.ErrUnexpectedEOF):
		reason = "ended before a complete HTTP response"
	}
	target, _ := c.safeResponseText(request.Method+" "+request.URL.Scheme+"://"+request.URL.Host+request.URL.Path, 512)
	return &requestFailure{message: "HTTP request " + target + " " + reason, cause: cause}
}
