// Package resources prepares immutable, owner-authorized compilation environments.
package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

var (
	ErrNotFound = errors.New("environment not found")
	ErrNotReady = errors.New("environment is not prepared")
	ErrQuota    = errors.New("resource cache quota exceeded")
	ErrExport   = errors.New("resource redistribution is not authorized")
)

type ownerAccess struct {
	Environment model.Environment `json:"environment"`
	Uploaded    map[string]bool   `json:"uploaded,omitempty"`
	Used        time.Time         `json:"used"`
}

type record struct {
	Environment model.Environment       `json:"environment"`
	Owners      map[string]*ownerAccess `json:"owners"`
	Prepared    bool                    `json:"prepared"`
	TreeFiles   []model.File            `json:"treeFiles,omitempty"`
	refs        int
	treeLeases  int
	busy        chan struct{}
}

// MaterializedFile is an authorized immutable input. Source is never accepted
// from an API request; Path is its checked logical project-relative destination.
type MaterializedFile struct {
	Path   string
	Source string
	SHA256 string
	Size   int64
	Kind   string
}

type Manager struct {
	mu        sync.Mutex
	cfg       config.Config
	dir       string
	baselines map[string]model.Baseline
	defaultID string
	catalog   map[string]catalogResource
	records   map[string]*record
	reserved  int64
	prepares  chan struct{}
}

// New loads registered baselines and durable authorization records. Unfinished
// publications are removed before requests can acquire a lease.
func New(cfg config.Config, meta protocol.Metadata) (*Manager, error) {
	resourceDefaults(&cfg)
	if cfg.MaxResourceBytes < 0 || cfg.MaxResourceBytes > math.MaxInt64/8 || cfg.MaxResourceFiles < 0 ||
		cfg.MaxResourceFiles > 1_000_000 ||
		cfg.MaxResourceEnvironments < 0 ||
		cfg.MaxConcurrentResourcePrepares < 0 ||
		cfg.ResourceCacheBytes < 0 ||
		cfg.ResourceOwnerBytes < 0 ||
		cfg.ResourceRetention < 0 ||
		cfg.ResourcePrepareTimeout < 0 {
		return nil, errors.New("invalid resource limits")
	}
	dir, err := cfg.ResourceDirectory()
	if err != nil {
		return nil, err
	}
	cfg.ResourceCacheDir = dir
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	root, err := safefs.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"records", "blobs", "trees", "staging", "metadata", "archives"} {
		if err := root.MakeDirs(name, 0700); err != nil {
			return nil, err
		}
	}
	// Atomic archive/index publications stage beside their destinations. A
	// process crash can leave those private siblings outside staging/, where
	// they must not consume the persistent cache budget until normal retention.
	for _, directory := range []string{"archives", "metadata"} {
		entries, err := root.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if safefs.IsStagedName(entry.Name()) {
				if err := root.Remove(directory + "/" + entry.Name()); err != nil {
					return nil, err
				}
			}
		}
	}
	m := &Manager{
		cfg:       cfg,
		dir:       dir,
		baselines: map[string]model.Baseline{},
		catalog:   map[string]catalogResource{},
		records:   map[string]*record{},
	}
	m.prepares = make(chan struct{}, cfg.MaxConcurrentResourcePrepares)
	base, err := defaultBaseline(cfg, meta)
	if err != nil {
		return nil, err
	}
	m.defaultID, m.baselines[base.ID] = base.ID, base
	if cfg.ResourceRegistryFile != "" {
		if err := m.loadRegistry(cfg.ResourceRegistryFile); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "records"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, errors.New("unexpected resource record")
		}
		digest := strings.TrimSuffix(entry.Name(), ".json")
		if !model.ValidDigest(digest) {
			return nil, errors.New("invalid resource record identity")
		}
		data, err := root.ReadLimited("records/"+entry.Name(), 16<<20)
		if err != nil {
			return nil, err
		}
		var rec record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, err
		}
		actual, err := model.EnvironmentDigest(rec.Environment)
		if err != nil || actual != digest || rec.Environment.Digest != digest || len(rec.Owners) == 0 {
			return nil, errors.New("corrupt environment record")
		}
		for key, access := range rec.Owners {
			if !model.ValidDigest(key) || access == nil {
				return nil, errors.New("corrupt environment authorization")
			}
			got, err := model.EnvironmentDigest(access.Environment)
			if err != nil || got != digest {
				return nil, errors.New("corrupt environment authorization")
			}
			if access.Uploaded == nil {
				access.Uploaded = map[string]bool{}
			}
		}
		m.records[digest] = &rec
	}
	staged, err := os.ReadDir(filepath.Join(dir, "staging"))
	if err != nil {
		return nil, err
	}
	for _, entry := range staged {
		if err := os.RemoveAll(filepath.Join(dir, "staging", entry.Name())); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func resourceDefaults(cfg *config.Config) {
	if cfg.MaxResourceBytes == 0 {
		cfg.MaxResourceBytes = 512 << 20
	}
	if cfg.MaxResourceFiles == 0 {
		cfg.MaxResourceFiles = 20000
	}
	if cfg.ResourceCacheBytes == 0 {
		cfg.ResourceCacheBytes = 4 << 30
	}
	if cfg.ResourceOwnerBytes == 0 {
		cfg.ResourceOwnerBytes = 1 << 30
	}
	if cfg.MaxResourceEnvironments == 0 {
		cfg.MaxResourceEnvironments = 64
	}
	if cfg.MaxConcurrentResourcePrepares == 0 {
		cfg.MaxConcurrentResourcePrepares = 2
	}
	if cfg.ResourceRetention == 0 {
		cfg.ResourceRetention = 7 * 24 * time.Hour
	}
	if cfg.ResourcePrepareTimeout == 0 {
		cfg.ResourcePrepareTimeout = 5 * time.Minute
	}
}

func copyBaseline(value model.Baseline) model.Baseline {
	value.Engines = slices.Clone(value.Engines)
	value.Toolchain = maps.Clone(value.Toolchain)
	value.Fonts = slices.Clone(value.Fonts)
	value.Packages = slices.Clone(value.Packages)
	return value
}

func copyResources(values []model.Resource) []model.Resource {
	values = slices.Clone(values)
	for i := range values {
		values[i].Packages = slices.Clone(values[i].Packages)
		values[i].Files = slices.Clone(values[i].Files)
		values[i].LicenseFiles = slices.Clone(values[i].LicenseFiles)
	}
	return values
}

func copyEnvironment(value model.Environment) model.Environment {
	value.Baseline = copyBaseline(value.Baseline)
	value.Resources = copyResources(value.Resources)
	value.Provided = copyResources(value.Provided)
	value.Requires.Fonts = slices.Clone(value.Requires.Fonts)
	value.Requires.Tools = slices.Clone(value.Requires.Tools)
	value.Packages = slices.Clone(value.Packages)
	value.Missing = slices.Clone(value.Missing)
	return value
}
func ownerKey(owner string) string { digest, _ := model.Hash(owner); return digest }

func (m *Manager) List() []model.Baseline {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := make([]model.Baseline, 0, len(m.baselines))
	for _, value := range m.baselines {
		values = append(values, copyBaseline(value))
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values
}

func (m *Manager) DefaultID() string { return m.defaultID }

// Resolve freezes package revisions and resource manifests before hashing the
// environment. Knowing another owner's content digest never grants blob access.
func (m *Manager) Resolve(ctx context.Context, owner string, request model.ResolveRequest) (model.Environment, error) {
	if owner == "" {
		return model.Environment{}, ErrNotFound
	}
	request, err := model.NormalizeRequest(request)
	if err != nil {
		return model.Environment{}, err
	}
	if request.Baseline == "" {
		request.Baseline = m.defaultID
	}
	baseline, ok := m.baselines[request.Baseline]
	if !ok {
		return model.Environment{}, errors.New("unknown baseline")
	}
	if baseline.Image == "" && baseline.ID != "default" {
		return model.Environment{}, errors.New("alternate baseline requires an isolated runner image")
	}
	request.Resources, err = m.expand(owner, request.Resources)
	if err != nil {
		return model.Environment{}, err
	}
	request.Provided, err = m.expand(owner, request.Provided)
	if err != nil {
		return model.Environment{}, err
	}
	request, err = model.NormalizeRequest(request)
	if err != nil {
		return model.Environment{}, err
	}
	if err = validateProvided(baseline, request.Provided); err != nil {
		return model.Environment{}, err
	}
	var total int64
	count := 0
	var requested []string
	sizes := map[string]int64{}
	for _, resource := range request.Resources {
		requested = append(requested, resource.Packages...)
		for _, file := range resource.Files {
			if previous, ok := sizes[file.SHA256]; ok && previous != file.Size {
				return model.Environment{}, errors.New("one content digest cannot declare different sizes")
			}
			sizes[file.SHA256] = file.Size
			if file.Size > m.cfg.MaxResourceBytes-total {
				return model.Environment{}, ErrQuota
			}
			total += file.Size
			count++
		}
	}
	if count > m.cfg.MaxResourceFiles {
		return model.Environment{}, ErrQuota
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ResourcePrepareTimeout)
	defer cancel()
	packages, err := m.resolvePackages(ctx, baseline, requested)
	if err != nil {
		return model.Environment{}, err
	}
	env := model.Environment{
		Baseline:  baseline,
		Resources: request.Resources,
		Provided:  request.Provided,
		Requires:  request.Requires,
		Packages:  packages,
	}
	env.Digest, err = model.EnvironmentDigest(env)
	if err != nil {
		return model.Environment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := ownerKey(owner)
	owned, ownedBytes := m.ownerUsageLocked(key)
	rec := m.records[env.Digest]
	newBytes := total
	if rec != nil {
		newBytes += filesBytes(rec.TreeFiles)
	}
	if (rec == nil || rec.Owners[key] == nil) &&
		(owned >= m.cfg.MaxResourceEnvironments || newBytes > m.cfg.ResourceOwnerBytes-ownedBytes) {
		return model.Environment{}, ErrQuota
	}
	if rec == nil {
		rec = &record{Environment: copyEnvironment(env), Owners: map[string]*ownerAccess{}}
	}
	prior := rec.Owners[key]
	access := prior
	if access == nil {
		access = &ownerAccess{Environment: copyEnvironment(env), Uploaded: map[string]bool{}}
		needed := map[string]bool{}
		for _, resource := range env.Resources {
			if resource.Type == "files" {
				for _, file := range resource.Files {
					needed[file.SHA256] = true
				}
			}
		}
		// Reuse only blobs this owner has already proved possession of.
		for _, other := range m.records {
			if prior := other.Owners[key]; prior != nil {
				for sha, possessed := range prior.Uploaded {
					if possessed && needed[sha] {
						access.Uploaded[sha] = true
					}
				}
			}
		}
	} else {
		copy := *access
		copy.Uploaded = maps.Clone(access.Uploaded)
		access = &copy
		access.Environment = copyEnvironment(env)
	}
	if access.Uploaded == nil {
		access.Uploaded = map[string]bool{}
	}
	access.Used = time.Now().UTC()
	for _, resource := range env.Resources {
		if resource.Type != "server" {
			continue
		}
		registered := m.catalog[resource.Ref]
		for _, file := range resource.Files {
			if err := m.importCatalogLocked(registered, file); err != nil {
				return model.Environment{}, err
			}
		}
	}
	rec.Owners[key] = access
	if err := m.saveLocked(rec); err != nil {
		if prior == nil {
			delete(rec.Owners, key)
		} else {
			rec.Owners[key] = prior
		}
		return model.Environment{}, err
	}
	m.records[env.Digest] = rec
	return m.viewLocked(rec, access), nil
}

func filesBytes(files []model.File) int64 {
	var n int64
	for _, file := range files {
		n += file.Size
	}
	return n
}

func (m *Manager) ownerUsageLocked(owner string) (int, int64) {
	var count int
	var bytes int64
	for _, rec := range m.records {
		if access := rec.Owners[owner]; access != nil {
			count++
			bytes += manifestBytes(access.Environment) + filesBytes(rec.TreeFiles)
		}
	}
	return count, bytes
}

func (m *Manager) resolvePackages(
	ctx context.Context,
	baseline model.Baseline,
	requested []string,
) ([]model.Package, error) {
	installed := map[string]bool{}
	for _, pkg := range baseline.Packages {
		installed[pkg.Name] = true
	}
	remote := false
	for _, name := range requested {
		if !installed[name] {
			remote = true
			break
		}
	}
	if !remote {
		return nil, nil
	}
	if baseline.Image != m.cfg.RunnerImage {
		return nil, errors.New(
			"additional TeX Live packages require the controller runtime; alternate baselines may use their installed packages only",
		)
	}
	select {
	case m.prepares <- struct{}{}:
		defer func() { <-m.prepares }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if index := m.cfg.ResourcePackageIndex; index != nil {
		// The asserted runtime index is read in place and never populates the
		// writable metadata cache. Admission still bounds concurrent parsing, and
		// the reader enforces its byte limit and snapshot/checksum binding.
		return resolvePackages(ctx, baseline, requested, "", maxPackageMetadataBytes, index)
	}
	m.mu.Lock()
	used, err := diskBytes(m.dir)
	limit := min(maxPackageMetadataBytes, m.cfg.ResourceCacheBytes-used-m.reserved)
	if err == nil && limit <= 0 {
		err = &model.AcquisitionError{
			Code:      "resource_cache_unavailable",
			Stage:     "resource-cache",
			Reason:    "capacity",
			Retryable: true,
			Cause:     ErrQuota,
		}
	}
	if err == nil {
		err = m.reserveLocked(limit)
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() { m.mu.Lock(); m.reserved -= limit; m.mu.Unlock() }()
	return resolvePackages(
		ctx,
		baseline,
		requested,
		filepath.Join(m.dir, "metadata"),
		limit,
		m.cfg.ResourcePackageIndex,
	)
}

func manifestBytes(env model.Environment) int64 {
	var n int64
	for _, r := range env.Resources {
		n += filesBytes(r.Files)
	}
	return n
}

func (m *Manager) Inspect(owner, digest string) (model.Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, access, err := m.authorizeLocked(owner, digest)
	if err != nil {
		return model.Environment{}, err
	}
	access.Used = time.Now().UTC()
	return m.viewLocked(rec, access), nil
}

// Authorize checks ownership without requiring preparation.
func (m *Manager) Authorize(owner, digest string) error {
	_, err := m.Inspect(owner, digest)
	return err
}

func (m *Manager) releaseRef(rec *record) { m.mu.Lock(); defer m.mu.Unlock(); rec.refs-- }

func (m *Manager) authorizeLocked(owner, digest string) (*record, *ownerAccess, error) {
	if owner == "" || !model.ValidDigest(digest) {
		return nil, nil, ErrNotFound
	}
	rec := m.records[digest]
	if rec == nil {
		return nil, nil, ErrNotFound
	}
	access := rec.Owners[ownerKey(owner)]
	if access == nil {
		return nil, nil, ErrNotFound
	}
	for _, resource := range append(slices.Clone(access.Environment.Resources), access.Environment.Provided...) {
		if resource.Type != "server" {
			continue
		}
		registered, ok := m.catalog[resource.Ref]
		if !ok || (len(registered.Owners) > 0 && !slices.Contains(registered.Owners, owner)) {
			return nil, nil, ErrNotFound
		}
		if !slices.Equal(registered.Files, resource.Files) {
			return nil, nil, ErrNotFound
		}
	}
	return rec, access, nil
}

func (m *Manager) viewLocked(rec *record, access *ownerAccess) model.Environment {
	env := copyEnvironment(access.Environment)
	env.Missing = nil
	seen := map[string]bool{}
	for _, resource := range env.Resources {
		for _, file := range resource.Files {
			if seen[file.SHA256] {
				continue
			}
			seen[file.SHA256] = true
			if (resource.Type != "server" && !access.Uploaded[file.SHA256]) || !m.hasBlob(file) {
				env.Missing = append(env.Missing, file.SHA256)
			}
		}
	}
	slices.Sort(env.Missing)
	env.Ready = rec.Prepared && len(env.Missing) == 0
	if env.Ready && needsTree(env) {
		info, err := os.Lstat(m.treePath(env.Digest))
		env.Ready = err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
	}
	return env
}

func (m *Manager) blobPath(sha string) string    { return filepath.Join(m.dir, "blobs", sha) }
func (m *Manager) treePath(digest string) string { return filepath.Join(m.dir, "trees", digest) }
func (m *Manager) hasBlob(file model.File) bool {
	root, err := safefs.Open(filepath.Join(m.dir, "blobs"))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenRegular(file.SHA256)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	return err == nil && info.Size() == file.Size
}

func (m *Manager) PutBlob(ctx context.Context, owner, digest, sha string, input io.Reader) error {
	m.mu.Lock()
	rec, access, err := m.authorizeLocked(owner, digest)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	file, found := declaredFile(access.Environment, sha, false)
	if !found {
		m.mu.Unlock()
		return ErrNotFound
	}
	if err = m.reserveLocked(file.Size); err != nil {
		m.mu.Unlock()
		return err
	}
	rec.refs++
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.reserved -= file.Size; rec.refs--; m.mu.Unlock() }()
	temp, err := os.CreateTemp(filepath.Join(m.dir, "staging"), "blob-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	err = safefs.CopyVerified(temp, contextReader{ctx: ctx, reader: input}, file.Size, file.SHA256)
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Chmod(name, 0444); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, access, err = m.authorizeLocked(owner, digest)
	if err != nil {
		return err
	}
	if err = os.Rename(name, m.blobPath(sha)); err != nil {
		return err
	}
	priorProof := access.Uploaded[sha]
	priorUsed := access.Used
	if access.Uploaded == nil {
		access.Uploaded = map[string]bool{}
	}
	access.Uploaded[sha] = true
	access.Used = time.Now().UTC()
	if err = m.saveLocked(rec); err != nil {
		if !priorProof {
			delete(access.Uploaded, sha)
		}
		access.Used = priorUsed
	}
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func declaredFile(env model.Environment, sha string, export bool) (model.File, bool) {
	for _, resource := range env.Resources {
		if export && resource.Redistribution != "allow" {
			continue
		}
		for _, file := range resource.Files {
			if file.SHA256 == sha {
				return file, true
			}
		}
	}
	return model.File{}, false
}

// OpenExport never exposes provided resources or a private resource merely
// because its content hash is known. The returned descriptor is hash-verified.
func (m *Manager) OpenExport(owner, digest, resourceID, sha string) (*os.File, model.File, error) {
	m.mu.Lock()
	rec, access, err := m.authorizeLocked(owner, digest)
	if err != nil {
		m.mu.Unlock()
		return nil, model.File{}, err
	}
	file, err := m.exportFileLocked(access, resourceID, sha)
	if err != nil {
		m.mu.Unlock()
		return nil, model.File{}, err
	}
	rec.refs++
	m.mu.Unlock()
	defer m.releaseRef(rec)
	f, err := verifiedOpen(m.blobPath(sha), file)
	if err != nil {
		return nil, model.File{}, err
	}
	m.mu.Lock()
	_, access, err = m.authorizeLocked(owner, digest)
	if err == nil {
		var latest model.File
		latest, err = m.exportFileLocked(access, resourceID, sha)
		if err == nil && latest != file {
			err = ErrExport
		}
	}
	if err == nil {
		access.Used = time.Now().UTC()
	}
	m.mu.Unlock()
	if err != nil {
		return nil, model.File{}, errors.Join(err, f.Close())
	}
	return f, file, nil
}

func (m *Manager) exportFileLocked(access *ownerAccess, resourceID, sha string) (model.File, error) {
	for _, resource := range access.Environment.Resources {
		if resource.ID != resourceID {
			continue
		}
		if resource.Redistribution != "allow" {
			return model.File{}, ErrExport
		}
		if resource.Type == "server" && m.catalog[resource.Ref].Redistribution != "allow" {
			return model.File{}, ErrExport
		}
		if resource.Type != "server" && !access.Uploaded[sha] {
			return model.File{}, ErrExport
		}
		for _, candidate := range resource.Files {
			if candidate.SHA256 == sha {
				return candidate, nil
			}
		}
	}
	return model.File{}, ErrExport
}

func verifiedOpen(source string, file model.File) (*os.File, error) {
	root, err := safefs.Open(filepath.Dir(source))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenRegular(filepath.Base(source))
	if err != nil {
		return nil, err
	}
	if err = safefs.CopyVerified(io.Discard, f, file.Size, file.SHA256); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (m *Manager) saveLocked(rec *record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return ErrQuota
	}
	if err = m.reserveLocked(int64(len(data))); err != nil {
		return err
	}
	defer func() { m.reserved -= int64(len(data)) }()
	temp, err := os.CreateTemp(filepath.Join(m.dir, "staging"), "record-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(m.dir, "records", rec.Environment.Digest+".json"))
}

func diskBytes(root string) (int64, error) {
	var n int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() {
				return errors.New("resource cache contains a nonregular file")
			}
			n += info.Size()
		}
		return nil
	})
	return n, err
}

func (m *Manager) reserveLocked(n int64) error {
	used, err := diskBytes(m.dir)
	if err != nil {
		return err
	}
	if n < 0 || n > m.cfg.ResourceCacheBytes {
		return ErrQuota
	}
	if n > m.cfg.ResourceCacheBytes-used-m.reserved {
		return &model.AcquisitionError{
			Code:      "resource_cache_unavailable",
			Stage:     "resource-cache",
			Reason:    "capacity",
			Retryable: true,
			Cause:     ErrQuota,
		}
	}
	m.reserved += n
	return nil
}

func needsTree(env model.Environment) bool {
	if len(env.Packages) > 0 {
		return true
	}
	for _, r := range env.Resources {
		if r.Kind == "texmf" && len(r.Files) > 0 {
			return true
		}
	}
	return false
}

// Start reclaims expired, unleased environments. Active jobs and preparation
// hold references, so collection cannot remove their inputs.
func (m *Manager) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if err := m.Sweep(now); err != nil {
					slog.WarnContext(ctx, "resource cache cleanup failed", "error", err)
				}
			}
		}
	}()
}

func (m *Manager) Sweep(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for digest, rec := range m.records {
		if rec.refs > 0 || rec.busy != nil {
			continue
		}
		remaining := maps.Clone(rec.Owners)
		for owner, access := range rec.Owners {
			if now.Sub(access.Used) > m.cfg.ResourceRetention {
				delete(remaining, owner)
			}
		}
		if len(remaining) == len(rec.Owners) {
			continue
		}
		if len(remaining) == 0 {
			if err := os.RemoveAll(m.treePath(digest)); err != nil {
				return err
			}
			if err := os.Remove(
				filepath.Join(m.dir, "records", digest+".json"),
			); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				return err
			}
			delete(m.records, digest)
		} else {
			updated := *rec
			updated.Owners = remaining
			if err := m.saveLocked(&updated); err != nil {
				return err
			}
			rec.Owners = remaining
		}
	}
	used := map[string]bool{}
	for _, rec := range m.records {
		for _, access := range rec.Owners {
			for _, r := range access.Environment.Resources {
				for _, f := range r.Files {
					if r.Type == "server" || access.Uploaded[f.SHA256] {
						used[f.SHA256] = true
					}
				}
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.dir, "blobs"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !used[entry.Name()] {
			if err := os.Remove(filepath.Join(m.dir, "blobs", entry.Name())); err != nil {
				return err
			}
		}
	}
	// Archive identities are SHA-512, independently referenced by every retained
	// environment. A failed preparation may keep verified downloads until expiry.
	archives := map[string]bool{}
	for _, rec := range m.records {
		for _, pkg := range rec.Environment.Packages {
			archives[pkg.Checksum+".tar.xz"] = true
		}
	}
	if m.reserved == 0 {
		for _, directory := range []string{"archives", "metadata"} {
			entries, err := os.ReadDir(filepath.Join(m.dir, directory))
			if err != nil {
				return err
			}
			for _, entry := range entries {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if directory == "archives" && archives[entry.Name()] {
					continue
				}
				if now.Sub(info.ModTime()) <= m.cfg.ResourceRetention {
					continue
				}
				if err := os.RemoveAll(filepath.Join(m.dir, directory, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *Manager) String() string {
	return fmt.Sprintf("resource environments (%d baselines)", len(m.baselines))
}
