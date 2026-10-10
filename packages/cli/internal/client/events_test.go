package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestSessionEventFraming(t *testing.T) {
	for _, test := range []struct {
		name, body string
		count      int
	}{
		{"optional space", "data:{\"sequence\":1}\n\n", 1},
		{"multiline", "data: {\n: heartbeat\ndata: \"sequence\":1\ndata: }\n\n", 1},
		{"CRLF", "data: {\"sequence\":1}\r\n\r\n", 1},
		{"CR", "data: {\"sequence\":1}\r\r", 1},
		{"BOM", "\ufeffdata: {\"sequence\":1}\n\n", 1},
		{"incomplete event", "data: {\"sequence\":1}\n", 0},
		{"comment and empty event", ": heartbeat\n\ndata: {\"sequence\":1}\n\n", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			err = c.StreamSessionEvents(context.Background(), "session", 0, func(event protocol.SessionEvent) {
				count++
				if event.Sequence != 1 {
					t.Errorf("sequence=%d", event.Sequence)
				}
			})
			if !errors.Is(err, io.EOF) || count != test.count {
				t.Fatalf("count=%d error=%v", count, err)
			}
		})
	}
}

func TestSessionEventRejectsInvalidContentTypeAndOversize(t *testing.T) {
	for _, test := range []struct{ name, contentType, body string }{
		{"prefix mimic", "text/event-stream-extra", ""},
		{"bad parameters", "text/event-stream; broken", ""},
		{"oversized multiline", "text/event-stream", strings.Repeat("data: "+strings.Repeat(" ", 1000)+"\n", 100) + "data: {\"sequence\":1}\n\n"},
		{"invalid JSON", "text/event-stream", "data: garbage\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			c, err := New(server.URL, "", time.Second, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.StreamSessionEvents(
				context.Background(),
				"session",
				0,
				func(protocol.SessionEvent) {},
			); err == nil ||
				errors.Is(err, io.EOF) {
				t.Fatalf("invalid stream accepted: %v", err)
			}
		})
	}
}

func TestSessionEventsDispatchCRWithoutWaitingForMoreBytes(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	received := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- readSessionEvents(reader, 0, func(protocol.SessionEvent) { received <- struct{}{} }) }()
	if _, err := io.WriteString(writer, "data: {\"sequence\":1}\r\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Error("CR terminated event waited for another network read")
	}
	_ = writer.Close()
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// shortEventReader simulates arbitrary transport chunk boundaries.
type shortEventReader struct {
	reader *bytes.Reader
	size   int
}

func (r shortEventReader) Read(p []byte) (int, error) { return r.reader.Read(p[:min(len(p), r.size)]) }

func FuzzSessionEventFraming(f *testing.F) {
	f.Add("submitted", uint8(1))
	f.Add("中文", uint8(4))
	f.Add("\r\n\x00", uint8(7))
	f.Fuzz(func(t *testing.T, eventType string, chunk uint8) {
		if len(eventType) > 4096 {
			t.Skip()
		}
		raw, err := json.Marshal(protocol.SessionEvent{Sequence: 1, Type: eventType})
		if err != nil {
			t.Fatal(err)
		}
		var expected protocol.SessionEvent
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		for _, ending := range []string{"\n", "\r\n", "\r"} {
			body := []byte("\ufeff: heartbeat" + ending + "data:" + string(raw) + ending + ending)
			count := 0
			err := readSessionEvents(
				shortEventReader{bytes.NewReader(body), int(chunk)%32 + 1},
				0,
				func(event protocol.SessionEvent) {
					count++
					if event != expected {
						t.Fatalf("event=%+v, expected=%+v", event, expected)
					}
				},
			)
			if !errors.Is(err, io.EOF) || count != 1 {
				t.Fatalf("count=%d, error=%v", count, err)
			}
		}
	})
}
