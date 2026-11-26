package client

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTypedResourceErrorRetainsOnlySafeBoundedDetails(t *testing.T) {
	c := &Client{Token: "private-token"}
	body := `{"error":"download failed private-token","code":"resource_upstream_format","retryable":false,"details":{
		"stage":"archive-fetch","reason":"html","package":"algorithmicx","revision":78101,
		"sourceId":"mirror-1","upstreamStatus":200,"contentType":"text/html","bytes":2402,
		"url":"https://secret.invalid/private-token","path":"/server/private","stderr":"secret",
		"attempts":[{"sourceId":"mirror-1","code":"resource_upstream_format","reason":"html","body":"<html>private-token</html>"}]}}`
	failure := c.readHTTPError(
		&http.Response{StatusCode: 502, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))},
	)
	if failure.ServerCode != "resource_upstream_format" || failure.Retryable == nil || *failure.Retryable {
		t.Fatalf("typed reason or retryability lost: %#v", failure)
	}
	if strings.Contains(failure.Message, c.Token) || failure.ResourceDetails["package"] != "algorithmicx" ||
		failure.ResourceDetails["bytes"] != int64(
			2402,
		) || failure.ResourceDetails["url"] != nil || failure.ResourceDetails["stderr"] != nil {
		t.Fatalf("unsafe or incomplete resource details: %#v", failure)
	}
	attempts := failure.ResourceDetails["attempts"].([]map[string]any)
	if len(attempts) != 1 || attempts[0]["body"] != nil {
		t.Fatalf("unbounded attempt details: %#v", attempts)
	}
	for _, unsafe := range []string{`"reason":"https://secret.invalid/"`, `"sourceId":"<html>secret</html>"`, `"expected":"secret"`} {
		detail := `{"error":"failure","code":"future_resource_code","retryable":false,"details":{` + unsafe + `}}`
		result := c.readHTTPError(
			&http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(detail)), Header: http.Header{}},
		)
		if len(result.ResourceDetails) != 0 {
			t.Fatalf("unsafe scalar forwarded: %#v", result.ResourceDetails)
		}
	}
}

func TestOldErrorShapeAndMalformedOptionalDiagnosticsRemainCompatible(t *testing.T) {
	c := &Client{}
	for _, body := range []string{`{"error":"old server failure"}`, `{"error":"old server failure","unknown":"ignored"}`} {
		failure := c.readHTTPError(
			&http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))},
		)
		if failure.Message != "old server failure" || failure.ServerCode != "" || failure.Retryable != nil ||
			failure.ResponseIssue != "" {
			t.Fatalf("old error contract changed: %#v", failure)
		}
	}
	long := strings.Repeat(`{"sourceId":"mirror-1"},`, 8) + `{"sourceId":"mirror-9"}`
	body := `{"error":"failure","code":"resource_upstream_http","details":{"attempts":[` + long + `]}}`
	failure := c.readHTTPError(
		&http.Response{StatusCode: 502, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))},
	)
	if failure.ResourceDetails["attempts"] != nil {
		t.Fatal("oversized attempts were forwarded")
	}
}
