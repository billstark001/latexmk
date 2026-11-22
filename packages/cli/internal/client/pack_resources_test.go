package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func TestExportEnvironmentResourceChecksPermissionAndIntegrity(t *testing.T) {
	for _, scenario := range []string{"allowed", "denied", "tampered"} {
		t.Run(scenario, func(t *testing.T) {
			contents := "font bytes"
			digest := sha256.Sum256([]byte(contents))
			sha := hex.EncodeToString(digest[:])
			environment := resources.Environment{
				Baseline: resources.Baseline{ID: "default", Digest: strings.Repeat("a", 64)},
				Resources: []resources.Resource{
					{ID: "font", Type: "server", Ref: "font-v1", Mount: "fonts/body", Redistribution: "allow",
						Files: []resources.File{{Path: "Body.otf", SHA256: sha, Size: int64(len(contents))}}},
				},
			}
			environment.Digest, _ = resources.EnvironmentDigest(environment)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls++
				if request.URL.Path != "/v1/environments/"+environment.Digest+"/resources/font/blobs/"+sha {
					t.Error(request.URL.Path)
				}
				if scenario == "tampered" {
					_, _ = w.Write([]byte("wrong font"))
				} else {
					_, _ = w.Write([]byte(contents))
				}
			}))
			defer server.Close()
			client, err := New(server.URL, "", 0, false)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "denied" {
				environment.Resources[0].Redistribution = "deny"
			}
			captured, err := client.ExportEnvironmentResource(context.Background(), environment, "font")
			if scenario != "allowed" {
				if err == nil {
					_ = captured.Close()
					t.Fatal("accepted unauthorized or changed bytes")
				}
				if scenario == "denied" && calls != 0 {
					t.Fatal("downloaded a denied resource")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = captured.Close() }()
			if len(captured.Files) != 1 || captured.Files[0].Path != "fonts/body/Body.otf" {
				t.Fatal(captured.Files)
			}
			bytes, err := projectarchive.ReadFile(captured.Files[0], 1024)
			if err != nil || string(bytes) != contents {
				t.Fatalf("bad captured export: %q %v", bytes, err)
			}
		})
	}
}

func TestResourceExportRejectsWholeInvalidManifestBeforeDownload(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for _, scenario := range []string{"late-size-limit", "parent-first", "child-first"} {
		t.Run(scenario, func(t *testing.T) {
			files := []resources.File{
				{Path: "body.otf", SHA256: sha, Size: 10},
				{Path: "large.otf", SHA256: sha, Size: 2 << 30},
			}
			if scenario == "parent-first" {
				files[0].Path, files[1].Path, files[1].Size = "font", "font/body.otf", 10
			} else if scenario == "child-first" {
				files[0].Path, files[1].Path, files[1].Size = "font/body.otf", "font", 10
			}
			environment := resources.Environment{
				Baseline: resources.Baseline{ID: "default", Digest: sha},
				Resources: []resources.Resource{{
					ID: "font", Type: "server", Ref: "font-v1", Redistribution: "allow", Files: files,
				}},
			}
			var err error
			environment.Digest, err = resources.EnvironmentDigest(environment)
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = w.Write([]byte("font bytes"))
			}))
			defer server.Close()
			client, err := New(server.URL, "", 0, false)
			if err != nil {
				t.Fatal(err)
			}
			captured, err := client.ExportEnvironmentResource(context.Background(), environment, "font")
			if captured != nil {
				_ = captured.Close()
			}
			if err == nil || requests != 0 {
				t.Fatalf("invalid export performed partial network work: requests=%d err=%v", requests, err)
			}
		})
	}
}
