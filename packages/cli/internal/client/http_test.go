package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIResponsesRejectTrailingNullAndOversizedBodies(t *testing.T) {
	for _, payload := range []string{"{} {}", "{} garbage", "null", "{}" + strings.Repeat(" ", 8<<20)} {
		t.Run(payload[:min(len(payload), 20)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetJob(context.Background(), "job_test"); err == nil {
				t.Fatal("accepted invalid API response")
			}
			if _, err := c.Metadata(context.Background()); err == nil {
				t.Fatal("accepted invalid metadata response")
			}
		})
	}
}

func TestMetadataAllowsNewFieldsButJobResponsesStayStrict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"futureField":true}`)
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Metadata(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetJob(context.Background(), "job_test"); err == nil {
		t.Fatal("job response accepted an unknown field")
	}
}
