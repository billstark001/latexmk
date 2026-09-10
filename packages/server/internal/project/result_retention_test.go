package project

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
)

func TestAuxiliaryRetentionAndExpiryPreserveFinalArtifacts(t *testing.T) {
	for _, local := range []string{"none", "cache", "output"} {
		for _, server := range []string{"none", "retain", "reuse"} {
			t.Run(local+"/"+server, func(t *testing.T) {
				m, _, _ := cacheFixture(t)
				output := cacheOutput(
					t,
					t.TempDir(),
					map[string]string{
						"main.pdf":        "pdf",
						"main.aux":        "aux",
						"main.log":        "log",
						"main.synctex.gz": "sync",
					},
				)
				req := api.CompileRequest{
					Auxiliary: api.AuxiliaryOptions{Local: local, Server: server, ServerTTL: "1h"},
				}
				retained := compile.RetainArtifacts(output, req, 24*time.Hour)
				want := 4
				if local == "none" && server == "none" {
					want = 3
				}
				if len(retained.Files) != want {
					t.Fatalf("retained %d files, want %d", len(retained.Files), want)
				}
				if len(output.Files) != 4 {
					t.Fatal("selection mutated compiler output needed for reuse")
				}
				if retained.Result.AuxiliaryExpiresAt != nil {
					expired := time.Now().Add(-time.Minute)
					retained.Result.AuxiliaryExpiresAt = &expired
				}
				path, err := m.WriteResult("alice", "job1", retained)
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.PruneResultAuxiliary("alice", "job1"); err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !before.ModTime().Equal(after.ModTime()) {
					t.Fatal("auxiliary expiry extended result lifetime")
				}
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				gz, err := gzip.NewReader(f)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = gz.Close() }()
				tr := tar.NewReader(gz)
				files := map[string]bool{}
				for {
					h, err := tr.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					files[h.Name] = true
					if h.Name == "result.json" {
						var result api.CompileResult
						if err := json.NewDecoder(tr).Decode(&result); err != nil {
							t.Fatal(err)
						}
						if len(result.Artifacts) != 3 {
							t.Fatalf("stale metadata: %+v", result.Artifacts)
						}
					}
				}
				if files["artifacts/main.aux"] || !files["artifacts/main.pdf"] || !files["artifacts/main.log"] ||
					!files["artifacts/main.synctex.gz"] {
					t.Fatalf("expiry contents: %v", files)
				}
				if m.stateBytes != after.Size() {
					t.Fatalf("quota accounting: %d != %d", m.stateBytes, after.Size())
				}
			})
		}
	}
}
