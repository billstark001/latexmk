package resources

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestArchiveAcquisitionLogsFallbackAndCacheProvenanceWithoutSources(t *testing.T) {
	data := append([]byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}, []byte("exact archive fixture")...)
	hash := sha512.Sum512(data)
	pkg := model.Package{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(hash[:])}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/blocked/") {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!DOCTYPE html>private challenge text"))
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	policy := &model.SourcePolicy{Version: 1, ArchiveMirrors: []string{server.URL + "/blocked", server.URL + "/good"}}
	var buffer bytes.Buffer
	ctx := WithAcquisitionLogger(context.Background(), slog.New(slog.NewJSONHandler(&buffer, nil)), "req_provenance")
	directory := t.TempDir()
	if _, _, err := cachedPackageArchive(ctx, directory, server.URL, pkg, 4096, server.Client(), policy); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cachedPackageArchive(ctx, directory, server.URL, pkg, 4096, server.Client(), policy); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one provenance event per acquisition: %s", buffer.String())
	}
	for i, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["request_id"] != "req_provenance" || event["package"] != pkg.Name || event["sha512"] != pkg.Checksum {
			t.Fatalf("missing request/content correlation: %#v", event)
		}
		if i == 0 &&
			(event["source_id"] != "mirror-2" || event["cache_hit"] != false || len(event["attempts"].([]any)) != 2) {
			t.Fatalf("missing successful fallback provenance: %#v", event)
		}
		if i == 1 && (event["source_id"] != "cache" || event["cache_hit"] != true) {
			t.Fatalf("missing cache provenance: %#v", event)
		}
	}
	if strings.Contains(buffer.String(), server.URL) || strings.Contains(buffer.String(), "private challenge") ||
		strings.Contains(buffer.String(), directory) {
		t.Fatalf("acquisition event disclosed transport details: %s", buffer.String())
	}
}
