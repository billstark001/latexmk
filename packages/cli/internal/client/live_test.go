package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

func TestPreparedRevisionReplaysExactOperationAfterAmbiguousResponse(t *testing.T) {
	prepared := PreparedRevision{
		SessionID: "ses_1",
		Payload:   protocol.RevisionRequest{UploadID: "upl_1", BaseRevision: 4, IdempotencyKey: "idempotency-key-1"},
	}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload protocol.RevisionRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(payload, prepared.Payload) {
			t.Errorf("operation changed: %+v", payload)
		}
		count++
		if count == 1 {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.Job{ID: "accepted-once", Revision: 5})
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CommitRevision(context.Background(), prepared); err == nil {
		t.Fatal("expected lost response")
	}
	job, err := c.CommitRevision(context.Background(), prepared)
	if err != nil || job.ID != "accepted-once" || count != 2 {
		t.Fatalf("retry=%+v %v", job, err)
	}
}

func TestSessionEventsReplaySkipsDuplicateSequence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "3" {
			t.Error("missing replay cursor")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"sequence\":3}\n\ndata: {\"sequence\":4,\"type\":\"resync\"}\n\n")
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.SessionEvent
	err = c.StreamSessionEvents(
		context.Background(),
		"ses_1",
		3,
		func(e protocol.SessionEvent) { events = append(events, e) },
	)
	if !errors.Is(err, io.EOF) || len(events) != 1 || events[0].Sequence != 4 {
		t.Fatalf("events=%+v %v", events, err)
	}
}

func TestLivePublicationKeepsLastGoodAndRejectsOlderOrTamperedBundle(t *testing.T) {
	pdf := []byte("verified pdf")
	digest := sha256.Sum256(pdf)
	result := protocol.CompileResult{
		ProtocolVersion: 2,
		RequestID:       "job_2",
		SessionID:       "ses_1",
		Revision:        2,
		SourceRoot:      "/work/project",
		Success:         true,
		Artifacts: []protocol.Artifact{
			{Path: "main.pdf", Size: int64(len(pdf)), SHA256: hex.EncodeToString(digest[:])},
		},
	}
	payload := pdf
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(result)
		_, _ = w.Write(
			buildResultArchive(
				t,
				[]tarEntry{
					{"result.json", raw},
					{"stdout.log", nil},
					{"stderr.log", nil},
					{"artifacts/main.pdf", payload},
				},
			),
		)
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	c.ProjectRoot = t.TempDir()
	output := t.TempDir()
	request := protocol.CompileRequest{Entry: "main.tex", Engine: "xelatex"}
	download := func() (string, error) {
		_, root, err := c.DownloadLiveResult(
			context.Background(),
			protocol.Job{ID: result.RequestID, SessionID: result.SessionID, Revision: result.Revision, Result: &result},
			request,
			output,
		)
		return root, err
	}
	generation, err := download()
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(filepath.Dir(generation), "current.json")
	good, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	var publication LivePublication
	if err := json.Unmarshal(good, &publication); err != nil {
		t.Fatal(err)
	}
	if publication.SourceRoot != c.ProjectRoot || publication.RemoteSourceRoot != result.SourceRoot {
		t.Fatalf("source mapping lost in publication: %+v", publication)
	}
	result.RequestID, result.Revision, result.Success = "job_3", 3, false
	if root, err := download(); err != nil || root != "" {
		t.Fatalf("failure replaced preview: %s %v", root, err)
	}
	result.RequestID, result.Revision, result.Success = "job_1", 1, true
	if _, err := download(); err == nil {
		t.Fatal("older revision published")
	}
	result.RequestID, result.Revision = "job_4", 4
	payload = []byte("malicious!!!")
	if _, err := download(); err == nil {
		t.Fatal("tampered artifact published")
	}
	latest, err := os.ReadFile(current)
	if err != nil || string(latest) != string(good) {
		t.Fatal("last good pointer changed")
	}
}

func TestSyncTeXRebaseConfinesLocalSourceMapping(t *testing.T) {
	root := t.TempDir()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(
		writer,
		"Input:1:/work/project/main.tex\nInput:2:./chapters/a.tex\nInput:3:../../secret\nInput:4:/texmf/article.cls\n",
	)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.synctex.gz"), compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	artifact, err := rebaseSyncTeX(root, protocol.Artifact{Path: "main.synctex.gz"}, "/work/project", "/local/project")
	if err != nil || artifact.SHA256 == "" {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(root, artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil ||
		string(
			data,
		) != "Input:1:/local/project/main.tex\nInput:2:/local/project/chapters/a.tex\nInput:3:../../secret\nInput:4:/texmf/article.cls\n" {
		t.Fatalf("mapping=%s %v", data, err)
	}
}

func TestSessionResyncCanResetAFutureCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			"data: {\"sequence\":2,\"type\":\"resync\"}\n\ndata: {\"sequence\":3,\"type\":\"submitted\"}\n\n",
		)
	}))
	defer server.Close()
	c, err := New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	err = c.StreamSessionEvents(
		context.Background(),
		"ses_1",
		100,
		func(e protocol.SessionEvent) { sequences = append(sequences, e.Sequence) },
	)
	if !errors.Is(err, io.EOF) || !reflect.DeepEqual(sequences, []uint64{2, 3}) {
		t.Fatalf("resync=%v %v", sequences, err)
	}
}
