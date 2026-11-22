package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := New(
		config.Config{
			StateDir:             t.TempDir(),
			ResourceInventoryDir: t.TempDir(),
			Engines:              []string{"xelatex"},
			MaxResourceBytes:     64 << 10,
			MaxResourceFiles:     100,
			ResourceCacheBytes:   2 << 20,
			ResourceOwnerBytes:   1 << 20,
		},
		protocol.Metadata{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
func resourceRequest(content string, kind string) model.ResolveRequest {
	sha := sha256.Sum256([]byte(content))
	return model.ResolveRequest{
		Resources: []model.Resource{
			{
				ID:             "paper-assets",
				Type:           "files",
				Kind:           kind,
				Mount:          "vendor",
				Redistribution: "allow",
				Files: []model.File{
					{Path: "asset.dat", SHA256: hex.EncodeToString(sha[:]), Size: int64(len(content))},
				},
			},
		},
	}
}

func uploadResource(
	t *testing.T,
	m *Manager,
	owner string,
	request model.ResolveRequest,
	content string,
) model.Environment {
	t.Helper()
	env, err := m.Resolve(context.Background(), owner, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.PutBlob(
		context.Background(),
		owner,
		env.Digest,
		request.Resources[0].Files[0].SHA256,
		strings.NewReader(content),
	); err != nil {
		t.Fatal(err)
	}
	env, err = m.Prepare(context.Background(), owner, env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestPossessionAuthorizationIntegrityAndLeaseCollection(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("font-data", "data")
	env, err := m.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Inspect("bob", env.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("environment disclosed to another owner: %v", err)
	}
	sha := request.Resources[0].Files[0].SHA256
	if err = m.PutBlob(context.Background(), "alice", env.Digest, sha, strings.NewReader("wrongdata")); err == nil {
		t.Fatal("accepted bytes with wrong checksum")
	}
	if _, err = os.Stat(m.blobPath(sha)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed upload published content: %v", err)
	}
	env = uploadResource(t, m, "alice", request, "font-data")
	other, err := m.Resolve(context.Background(), "bob", request)
	if err != nil {
		t.Fatal(err)
	}
	if other.Ready || len(other.Missing) != 1 {
		t.Fatalf("knowledge of content hash granted possession: %+v", other)
	}
	if _, _, err = m.OpenExport("bob", other.Digest, "paper-assets", sha); !errors.Is(err, ErrExport) {
		t.Fatalf("private bytes exported: %v", err)
	}
	lease, err := m.Pin("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Files(); len(got) != 1 || got[0].Path != "vendor/asset.dat" {
		t.Fatalf("wrong logical materialization: %+v", got)
	}
	if err = m.Sweep(time.Now().Add(30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Inspect("alice", env.Digest); err != nil {
		t.Fatalf("collector removed leased input: %v", err)
	}
	lease.Release()
	lease.Release()
	if err = m.Sweep(time.Now().Add(30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Inspect("alice", env.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired lease retained environment: %v", err)
	}
	if _, err = os.Stat(m.blobPath(sha)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced blob retained: %v", err)
	}
}

func TestRestartPreservesProofsAndRejectsSameLengthCorruption(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("original", "data")
	env := uploadResource(t, m, "alice", request, "original")
	restarted, err := New(m.cfg, protocol.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := restarted.Pin("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	path := m.blobPath(request.Resources[0].Files[0].SHA256)
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Pin("alice", env.Digest); err == nil {
		t.Fatal("same-length corrupt blob reached compilation")
	}
	if _, _, err = restarted.OpenExport(
		"alice",
		env.Digest,
		"paper-assets",
		request.Resources[0].Files[0].SHA256,
	); err == nil {
		t.Fatal("same-length corrupt blob exported")
	}
}

type gatedReader struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	reader  io.Reader
}

func (r *gatedReader) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.release })
	return r.reader.Read(data)
}

func TestConcurrentResolveDoesNotDiscardSuccessfulUploadProof(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("race", "data")
	env, err := m.Resolve(context.Background(), "alice", request)
	if err != nil {
		t.Fatal(err)
	}
	reader := &gatedReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		reader:  strings.NewReader("race"),
	}
	done := make(chan error, 1)
	go func() {
		done <- m.PutBlob(context.Background(), "alice", env.Digest, request.Resources[0].Files[0].SHA256, reader)
	}()
	<-reader.started
	if _, err = m.Resolve(context.Background(), "alice", request); err != nil {
		t.Fatal(err)
	}
	close(reader.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	current, err := m.Inspect("alice", env.Digest)
	if err != nil || len(current.Missing) != 0 {
		t.Fatalf("successful upload ownership was lost: %+v %v", current, err)
	}
}

func TestTEXMFTreeIntegrityAndProvidedDependencies(t *testing.T) {
	m := testManager(t)
	request := resourceRequest("macro", "texmf")
	request.Resources[0].Mount = "."
	request.Resources[0].Files[0].Path = "tex/latex/paper/paper.sty"
	env := uploadResource(t, m, "alice", request, "macro")
	lease, err := m.Pin("alice", env.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.Files()) != 0 || lease.Tree() == "" {
		t.Fatal("TEXMF resources leaked into project-file materialization")
	}
	lease.Release()
	if err = os.WriteFile(filepath.Join(m.treePath(env.Digest), "extra.sty"), []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Pin("alice", env.Digest); err == nil {
		t.Fatal("undeclared prepared-tree file reached compilation")
	}
	provided := model.ResolveRequest{
		Provided: []model.Resource{{ID: "algorithms", Type: "texlive", Packages: []string{"algorithms"}}},
	}
	if _, err = m.Resolve(context.Background(), "alice", provided); err == nil {
		t.Fatal("missing provided package was silently added")
	}
	base := m.baselines[m.defaultID]
	base.Packages = []model.Package{{Name: "algorithms", Revision: 1}}
	base.Digest, err = model.BaselineDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	m.baselines[m.defaultID] = base
	if _, err = m.Resolve(context.Background(), "alice", provided); err != nil {
		t.Fatalf("installed provided package rejected: %v", err)
	}
	provided.Provided = request.Resources
	if _, err = m.Resolve(context.Background(), "alice", provided); err == nil {
		t.Fatal("unproved file paths were accepted as baseline-provided")
	}
}

func TestBaselineIdentityIsRuntimeScoped(t *testing.T) {
	cfg := config.Config{ResourceInventoryDir: t.TempDir(), Engines: []string{"xelatex"}, ImageProfile: "slim"}
	first, err := defaultBaseline(
		cfg,
		protocol.Metadata{Version: "one", Commit: "a", Toolchain: map[string]string{"xelatex": "version1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := defaultBaseline(
		cfg,
		protocol.Metadata{Version: "two", Commit: "b", Toolchain: map[string]string{"xelatex": "version1"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatal("application-only upgrade invalidated resource locks")
	}
	third, _ := defaultBaseline(cfg, protocol.Metadata{Toolchain: map[string]string{"xelatex": "version2"}})
	if first.Digest == third.Digest {
		t.Fatal("runtime tool change did not invalidate resource identity")
	}
}

func TestRecordMetadataCannotExceedGlobalQuota(t *testing.T) {
	m := testManager(t)
	m.cfg.ResourceCacheBytes = 12 << 10
	for index := 0; index < 64; index++ {
		request := resourceRequest("x", "data")
		request.Resources[0].ID = strings.Repeat("x", index+1)
		_, err := m.Resolve(context.Background(), "alice", request)
		if errors.Is(err, ErrQuota) {
			size, err := diskBytes(m.dir)
			if err != nil {
				t.Fatal(err)
			}
			if size > m.cfg.ResourceCacheBytes {
				t.Fatalf("metadata exceeded cache quota: %d", size)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("metadata records bypassed global cache quota")
}

func TestFontInventoryUsesLogicalNamesAndExactRequirements(t *testing.T) {
	dir := t.TempDir()
	hash := strings.Repeat("a", 64)
	if err := os.WriteFile(
		filepath.Join(dir, "fonts.sha256"),
		[]byte(hash+"  /system/private/Paper.ttf\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "fonts.tsv"),
		[]byte(
			"file\tindex\tfamily\tstyle\tpostscript_name\tfont_version\n/system/private/Paper.ttf\t2\tPaper,Paper Alias\tBold\tPaper-Bold\t1\n",
		),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	base, err := defaultBaseline(config.Config{ResourceInventoryDir: dir}, protocol.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Fonts) != 1 || base.Fonts[0].File != "Paper.ttf" || base.Fonts[0].SHA256 != hash ||
		base.Fonts[0].Index != 2 {
		t.Fatalf("wrong or leaking inventory: %+v", base.Fonts)
	}
	m := testManager(t)
	env := model.Environment{
		Baseline: base,
		Requires: model.Requirements{Fonts: []model.FontRequirement{{Family: "Paper Alias", Style: "Bold"}}},
	}
	if err = m.checkRequirements(context.Background(), env, ""); err != nil {
		t.Fatal(err)
	}
	env.Requires.Fonts[0].Style = "Italic"
	if err = m.checkRequirements(context.Background(), env, ""); err == nil {
		t.Fatal("fallback font satisfied an absent style")
	}
	env.Baseline.FontLookup = "files"
	if err = m.checkRequirements(context.Background(), env, ""); err == nil {
		t.Fatal("filename-only target accepted a named font dependency")
	}
}

func TestProjectMayRestrictCatalogRedistributionButCannotLiftIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "font.otf"), []byte("font bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	m := newReviewManager(
		t,
		[]catalogResource{
			{Ref: "public-font", Directory: dir, Kind: "font", Redistribution: "allow"},
			{Ref: "private-font", Directory: dir, Kind: "font", Redistribution: "deny"},
		},
	)
	for _, value := range []struct{ ref, policy, want string }{{"public-font", "deny", "deny"}, {"public-font", "unknown", "unknown"}, {"private-font", "allow", "deny"}, {"public-font", "", "allow"}} {
		env, err := m.Resolve(
			context.Background(),
			"alice",
			model.ResolveRequest{
				Resources: []model.Resource{
					{ID: "fonts", Type: "server", Ref: value.ref, Redistribution: value.policy},
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if env.Resources[0].Redistribution != value.want {
			t.Fatalf("project %s / registry %s became %s", value.policy, value.ref, env.Resources[0].Redistribution)
		}
		file, _, err := m.OpenExport("alice", env.Digest, "fonts", env.Resources[0].Files[0].SHA256)
		if file != nil {
			_ = file.Close()
		}
		if value.want == "allow" && err != nil {
			t.Fatal(err)
		}
		if value.want != "allow" && !errors.Is(err, ErrExport) {
			t.Fatalf("project restriction lifted: %v", err)
		}
	}
}

func TestRegisteredImageBaselinesRequireAnIsolatedController(t *testing.T) {
	registered := registry{
		Baselines: []model.Baseline{
			{
				ID:         "submission-v1",
				Image:      "registry.example/latexmk@sha256:" + strings.Repeat("b", 64),
				Engines:    []string{"lualatex"},
				FontLookup: "files",
			},
		},
	}
	digest, err := model.BaselineDigest(registered.Baselines[0])
	if err != nil {
		t.Fatal(err)
	}
	registered.Baselines[0].Digest = digest
	data, err := json.Marshal(registered)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "registry.json")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		StateDir:             t.TempDir(),
		ResourceInventoryDir: t.TempDir(),
		ResourceRegistryFile: file,
		Engines:              []string{"xelatex"},
	}
	if _, err = New(cfg, protocol.Metadata{}); err == nil || !strings.Contains(err.Error(), "isolated controller") {
		t.Fatalf("native controller registered a Docker baseline: %v", err)
	}
	cfg.RunnerImage = "registry.example/latexmk@sha256:" + strings.Repeat("c", 64)
	manager, err := New(cfg, protocol.Metadata{})
	if err != nil {
		t.Fatalf("isolated controller rejected registered image: %v", err)
	}
	if len(manager.List()) != 2 {
		t.Fatal("registered image inventory was not loaded")
	}
}
