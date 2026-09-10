package project

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
)

func cacheFixture(t *testing.T) (*Manager, Snapshot, string) {
	t.Helper()
	m, err := New(
		config.Config{
			StateDir:              t.TempDir(),
			MaxStateBytes:         1 << 20,
			MaxCompileCacheBytes:  4096,
			MaxFiles:              100,
			CompileCacheRetention: time.Hour,
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	s := Snapshot{
		OwnerID:   "alice",
		ProjectID: "paper",
		Files: []api.ProjectFile{
			{Path: "main.tex", SHA256: strings.Repeat("a", 64), Size: 1},
			{Path: "refs.bib", SHA256: strings.Repeat("b", 64), Size: 1},
		},
	}
	key := CompileCacheKey(api.CompileRequest{Entry: "main.tex", Engine: "xelatex"}, api.Metadata{}, "")
	return m, s, key
}

func cacheOutput(t *testing.T, root string, data map[string]string) compile.Output {
	t.Helper()
	out := compile.Output{Result: api.CompileResult{Success: true}}
	for name, content := range data {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(content))
		out.Files = append(
			out.Files,
			compile.File{
				RelativePath: name,
				AbsolutePath: path,
				Size:         int64(len(content)),
				SHA256:       hex.EncodeToString(hash[:]),
			},
		)
	}
	return out
}

func TestCompileCacheWarmStartAndInvalidation(t *testing.T) {
	m, s, key := cacheFixture(t)
	root := t.TempDir()
	out := cacheOutput(
		t,
		root,
		map[string]string{
			"main.aux":         "labels",
			"main.bbl":         "bibliography",
			"main.pdf":         "pdf",
			"main.fdb_latexmk": "old state",
			"main.fls":         "old paths",
		},
	)
	if n, err := m.SaveCompileCache(s, key, root, "job1", time.Now(), out); err != nil || n != 2 {
		t.Fatalf("save: %d %v", n, err)
	}
	dst := t.TempDir()
	if info := m.RestoreCompileCache(s, key, dst); info.Status != "hit" || info.RestoredFiles != 2 {
		t.Fatalf("restore: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dst, "main.pdf")); !os.IsNotExist(err) {
		t.Fatal("restored final PDF")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "main.aux")); string(data) != "labels" {
		t.Fatal("wrong aux")
	}
	changed := s
	changed.Files = append([]api.ProjectFile(nil), s.Files...)
	changed.Files[0].SHA256 = strings.Repeat("c", 64)
	if info := m.RestoreCompileCache(changed, key, t.TempDir()); info.Status != "hit" {
		t.Fatalf("TeX edits should reuse: %+v", info)
	}
	changed.Files[1].SHA256 = strings.Repeat("d", 64)
	if info := m.RestoreCompileCache(changed, key, t.TempDir()); info.Status != "miss" {
		t.Fatal("bibliography edit should invalidate")
	}
	changed.Files = changed.Files[:1]
	if info := m.RestoreCompileCache(changed, key, t.TempDir()); info.Status != "miss" {
		t.Fatal("removed input should invalidate")
	}
	for _, other := range []Snapshot{{OwnerID: "bob", ProjectID: s.ProjectID, Files: s.Files}, {OwnerID: s.OwnerID, ProjectID: "other", Files: s.Files}} {
		if info := m.RestoreCompileCache(other, key, t.TempDir()); info.Status != "miss" {
			t.Fatal("cross-owner/project hit")
		}
	}
}

func TestCompileCacheKeyIsolation(t *testing.T) {
	req := api.CompileRequest{Entry: "main.tex", Engine: "xelatex", Synctex: true}
	meta := api.Metadata{Toolchain: map[string]string{"xelatex": "v1"}}
	key := CompileCacheKey(req, meta, "")
	for _, edit := range []func(*api.CompileRequest){func(r *api.CompileRequest) { r.Entry = "other.tex" }, func(r *api.CompileRequest) { r.Engine = "pdflatex" }, func(r *api.CompileRequest) { r.JobName = "other" }, func(r *api.CompileRequest) { r.ShellEscape = true }, func(r *api.CompileRequest) { r.Synctex = false }} {
		other := req
		edit(&other)
		if CompileCacheKey(other, meta, "") == key {
			t.Fatal("request option not isolated")
		}
	}
	meta.Toolchain["xelatex"] = "v2"
	if CompileCacheKey(req, meta, "") == key {
		t.Fatal("toolchain not isolated")
	}
	meta.Toolchain["xelatex"] = "v1"
	if CompileCacheKey(req, meta, "updated packages") == key {
		t.Fatal("epoch not isolated")
	}
	req.Force = true
	req.Quiet = true
	if CompileCacheKey(req, meta, "") != key {
		t.Fatal("force must refresh same cache slot")
	}
}

func TestCompileCacheFailureConcurrencyAndRestart(t *testing.T) {
	m, s, key := cacheFixture(t)
	root := t.TempDir()
	now := time.Now()
	out := cacheOutput(t, root, map[string]string{"main.aux": "new"})
	if _, err := m.SaveCompileCache(s, key, root, "new", now, out); err != nil {
		t.Fatal(err)
	}
	oldRoot := t.TempDir()
	old := cacheOutput(t, oldRoot, map[string]string{"main.aux": "old"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.SaveCompileCache(s, key, oldRoot, "old", now.Add(-time.Second), old)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	old.Result.Success = false
	if n, err := m.SaveCompileCache(s, key, oldRoot, "failed", now.Add(time.Second), old); n != 0 || err != nil {
		t.Fatal("failed compile published")
	}
	old.Result.Success = true
	old.Result.TimedOut = true
	if n, err := m.SaveCompileCache(s, key, oldRoot, "timeout", now.Add(time.Second), old); n != 0 || err != nil {
		t.Fatal("timeout published")
	}
	restarted, err := New(m.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if info := restarted.RestoreCompileCache(s, key, dst); info.Status != "hit" {
		t.Fatal(info)
	}
	data, _ := os.ReadFile(filepath.Join(dst, "main.aux"))
	if string(data) != "new" {
		t.Fatal("older job replaced newer success")
	}
}

func TestCompileCacheRejectsCorruptionAndDoesNotOverwriteSources(t *testing.T) {
	m, s, key := cacheFixture(t)
	root := t.TempDir()
	out := cacheOutput(t, root, map[string]string{"main.aux": "labels"})
	if _, err := m.SaveCompileCache(s, key, root, "job", time.Now(), out); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "main.aux"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if info := m.RestoreCompileCache(s, key, dst); info.Status != "miss" {
		t.Fatal("overwrote source")
	}
	data, _ := os.ReadFile(filepath.Join(dst, "main.aux"))
	if string(data) != "source" {
		t.Fatal("source changed")
	}
	path, _ := m.compileCachePath(s, key)
	record, err := m.readCompileCache(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*compileCacheRecord){
		func(r *compileCacheRecord) { r.Files[0].Data = []byte("corrupt") },
		func(r *compileCacheRecord) { r.Files[0].Path = "../escape.aux" },
		func(r *compileCacheRecord) { r.Files[0].Path = "main.tex" },
	} {
		copyRecord := record
		copyRecord.Files = append([]auxiliaryFile(nil), record.Files...)
		mutate(&copyRecord)
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if err := json.NewEncoder(gz).Encode(copyRecord); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if info := m.RestoreCompileCache(s, key, t.TempDir()); info.Status != "miss" {
			t.Fatal("invalid cache accepted")
		}
	}
}

func TestCompileCacheSymlinksQuotasExpiryAndCleanup(t *testing.T) {
	m, s, key := cacheFixture(t)
	root := t.TempDir()
	out := cacheOutput(t, root, map[string]string{"sub/main.aux": "labels"})
	if _, err := m.SaveCompileCache(s, key, root, "job", time.Now(), out); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Fatal(err)
	}
	if info := m.RestoreCompileCache(s, key, dst); info.Status != "miss" {
		t.Fatal("followed symlink")
	}
	if files, _ := os.ReadDir(outside); len(files) != 0 {
		t.Fatal("wrote outside workspace")
	}
	count, size, _, err := m.CompileCacheStats(s.OwnerID, s.ProjectID)
	if err != nil || count != 1 || size == 0 {
		t.Fatalf("stats %d %d %v", count, size, err)
	}
	m.cfg.MaxCompileCacheBytes = 1
	if _, err := m.SaveCompileCache(s, key, root, "job2", time.Now(), out); err == nil {
		t.Fatal("ignored cache quota")
	}
	m.cfg.MaxCompileCacheBytes = 4096
	m.cfg.MaxStateBytes = m.stateBytes
	if _, err := m.SaveCompileCache(s, key, root, "job2", time.Now(), out); err == nil {
		t.Fatal("ignored global quota")
	}
	path, _ := m.compileCachePath(s, key)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if info := m.RestoreCompileCache(s, key, t.TempDir()); info.Status != "miss" {
		t.Fatal("restored expired cache")
	}
	if reclaimed, err := m.Prune(context.Background()); err != nil || reclaimed != size {
		t.Fatalf("prune %d %v", reclaimed, err)
	}
	m.cfg.MaxStateBytes = 1 << 20
	if _, err := m.SaveCompileCache(s, key, root, "job3", time.Now(), out); err != nil {
		t.Fatal(err)
	}
	if size, err := m.DeleteCompileCaches(s.OwnerID, s.ProjectID); err != nil || size == 0 {
		t.Fatalf("delete %d %v", size, err)
	}
	if info := m.RestoreCompileCache(s, key, t.TempDir()); info.Status != "miss" {
		t.Fatal("cache survived cleanup")
	}
}
