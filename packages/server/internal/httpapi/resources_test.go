package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestEnvironmentRoutesPrepareExactBytesAndRespectExportPolicy(t *testing.T) {
	s := newTestServer(t, false)
	call := func(method, path string, body []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewReader(body)))
		return recorder
	}
	list := call(http.MethodGet, "/v1/environments", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"default"`) {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	content := []byte("private institutional data")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	request := model.ResolveRequest{
		Resources: []model.Resource{
			{
				ID:             "paper-data",
				Type:           "files",
				Kind:           "data",
				Mount:          "data",
				Redistribution: "allow",
				Files:          []model.File{{Path: "values.dat", SHA256: sha, Size: int64(len(content))}},
			},
		},
	}
	raw, _ := json.Marshal(request)
	resolved := call(http.MethodPost, "/v1/environments/resolve", raw)
	var env model.Environment
	if resolved.Code != http.StatusOK || json.Unmarshal(resolved.Body.Bytes(), &env) != nil {
		t.Fatalf("resolve: %d %s", resolved.Code, resolved.Body.String())
	}
	base := "/v1/environments/" + env.Digest
	if got := call(http.MethodPost, base+"/prepare", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("prepared missing bytes: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPut, base+"/blobs/"+sha, []byte("incorrect")); got.Code != http.StatusBadRequest {
		t.Fatalf("accepted wrong bytes: %d", got.Code)
	}
	if got := call(http.MethodPut, base+"/blobs/"+sha, content); got.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", got.Code, got.Body.String())
	}
	ready := call(http.MethodPost, base+"/prepare", nil)
	if ready.Code != http.StatusOK || json.Unmarshal(ready.Body.Bytes(), &env) != nil || !env.Ready {
		t.Fatalf("prepare: %d %s", ready.Code, ready.Body.String())
	}
	if got := call(
		http.MethodGet,
		base+"/resources/paper-data/blobs/"+sha,
		nil,
	); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), content) ||
		got.Header().Get("X-Content-SHA256") != sha {
		t.Fatalf("export: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodGet, base+"/resources/unbound-id/blobs/"+sha, nil); got.Code != http.StatusForbidden {
		t.Fatalf("unbound resource exported: %d", got.Code)
	}
	request.Resources[0].Redistribution = "deny"
	raw, _ = json.Marshal(request)
	if got := call(http.MethodPost, "/v1/environments/resolve", raw); got.Code != http.StatusOK {
		t.Fatal(got.Body.String())
	}
	if got := call(http.MethodGet, base+"/resources/paper-data/blobs/"+sha, nil); got.Code != http.StatusForbidden {
		t.Fatalf("denied resource exported: %d", got.Code)
	}
}
