package resources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	model "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func (m *Manager) Prepare(ctx context.Context, owner, digest string) (model.Environment, error) {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ResourcePrepareTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return model.Environment{}, err
	}
	select {
	case m.prepares <- struct{}{}:
		defer func() { <-m.prepares }()
	case <-ctx.Done():
		return model.Environment{}, ctx.Err()
	}
	for {
		m.mu.Lock()
		rec, access, err := m.authorizeLocked(owner, digest)
		if err != nil {
			m.mu.Unlock()
			return model.Environment{}, err
		}
		view := m.viewLocked(rec, access)
		if view.Ready {
			m.mu.Unlock()
			return view, nil
		}
		if len(view.Missing) > 0 {
			m.mu.Unlock()
			return view, errors.New("environment has missing resource blobs")
		}
		if pending := rec.busy; pending != nil {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return model.Environment{}, ctx.Err()
			case <-pending:
				continue
			}
		}
		reserve := m.preparationBytes(view)
		if err = m.reserveLocked(reserve); err != nil {
			m.mu.Unlock()
			return model.Environment{}, err
		}
		rec.busy = make(chan struct{})
		rec.refs++
		m.mu.Unlock()

		tree, prepareErr := m.prepareTree(ctx, view)
		if prepareErr == nil {
			prepareErr = ctx.Err()
		}
		m.mu.Lock()
		m.reserved -= reserve
		// Resolve may have replaced the owner's policy while preparation ran.
		_, access, err = m.authorizeLocked(owner, digest)
		if err == nil {
			err = prepareErr
		}
		if err == nil {
			err = m.publishPreparedLocked(rec, access, tree)
		}
		if access != nil {
			view = m.viewLocked(rec, access)
		} else {
			view = model.Environment{}
		}
		rec.refs--
		close(rec.busy)
		rec.busy = nil
		m.mu.Unlock()
		return view, errors.Join(err, tree.remove())
	}
}

func (m *Manager) preparationBytes(env model.Environment) int64 {
	if len(env.Packages) > 0 {
		return 4*m.cfg.MaxResourceBytes + int64(m.cfg.MaxResourceFiles)*2048 + (1 << 20)
	}
	if needsTree(env) {
		return manifestBytes(env)
	}
	return 0
}

type preparedTree struct {
	directory string
	files     []model.File
}

func (t preparedTree) remove() error {
	if t.directory == "" {
		return nil
	}
	return os.RemoveAll(t.directory)
}

// publishPreparedLocked only replaces an unready tree; a ready environment
// returns before staging. If persistence fails, the replacement is removed and
// stays unprepared rather than restoring metadata for an absent old tree.
func (m *Manager) publishPreparedLocked(rec *record, access *ownerAccess, tree preparedTree) error {
	added := filesBytes(tree.files) - filesBytes(rec.TreeFiles)
	for owner := range rec.Owners {
		_, used := m.ownerUsageLocked(owner)
		if added > m.cfg.ResourceOwnerBytes-used {
			return ErrQuota
		}
	}
	target := m.treePath(rec.Environment.Digest)
	if tree.directory != "" {
		if rec.refs > 1 {
			return errors.New("cannot replace a resource tree while it is leased")
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.Rename(tree.directory, target); err != nil {
			return err
		}
	}
	oldPrepared, oldFiles, oldUsed := rec.Prepared, rec.TreeFiles, access.Used
	rec.Prepared, rec.TreeFiles, access.Used = true, tree.files, time.Now().UTC()
	if err := m.saveLocked(rec); err != nil {
		rec.Prepared, rec.TreeFiles, access.Used = oldPrepared, oldFiles, oldUsed
		if tree.directory != "" {
			rec.Prepared, rec.TreeFiles = false, nil
			err = errors.Join(err, os.RemoveAll(target))
		}
		return err
	}
	return nil
}

func (m *Manager) prepareTree(ctx context.Context, env model.Environment) (tree preparedTree, err error) {
	for _, resource := range env.Resources {
		for _, file := range resource.Files {
			f, err := verifiedOpen(m.blobPath(file.SHA256), file)
			if err != nil {
				return tree, err
			}
			if err := f.Close(); err != nil {
				return tree, err
			}
		}
	}
	if !needsTree(env) {
		return tree, m.checkRequirements(ctx, env, "")
	}
	stage, err := os.MkdirTemp(filepath.Join(m.dir, "staging"), "tree-")
	if err != nil {
		return tree, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	if err = preparePackages(
		ctx,
		env.Baseline,
		env.Packages,
		filepath.Join(m.dir, "archives"),
		stage,
		m.cfg.MaxResourceBytes,
		m.cfg.MaxResourceFiles,
	); err != nil {
		return tree, err
	}
	root, err := safefs.Open(stage)
	if err != nil {
		return tree, err
	}
	defer func() { _ = root.Close() }()
	for _, resource := range env.Resources {
		if resource.Kind != "texmf" {
			continue
		}
		for _, file := range resource.Files {
			name, err := model.Destination(resource.Mount, file.Path)
			if err != nil {
				return tree, err
			}
			if err = mergeTEXMFFile(root, name, file, m.blobPath(file.SHA256)); err != nil {
				return tree, err
			}
		}
	}
	files, err := inventoryFiles(stage, m.cfg.MaxResourceBytes-manifestBytes(env), m.cfg.MaxResourceFiles)
	if err != nil {
		return tree, err
	}
	if err = m.checkRequirements(ctx, env, stage); err != nil {
		return tree, err
	}
	return preparedTree{directory: stage, files: files}, nil
}

// mergeTEXMFFile preserves provenance in the environment while giving each
// installed path one immutable file, including paths supplied by TeX Live.
func mergeTEXMFFile(root *safefs.Root, name string, file model.File, sourcePath string) error {
	clean, err := safefs.Clean(name)
	if err != nil || clean != name {
		return errors.New("invalid TEXMF resource path")
	}
	if existing, err := root.OpenRegular(name); err == nil {
		check := safefs.CopyVerified(io.Discard, existing, file.Size, file.SHA256)
		if err = errors.Join(check, existing.Close()); err != nil {
			return fmt.Errorf("TEXMF resource collision at %q: %w", name, err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("TEXMF resource collision at %q: %w", name, err)
	}
	if err = root.MakeDirs(filepath.ToSlash(filepath.Dir(name)), 0700); err != nil {
		return err
	}
	source, err := verifiedOpen(sourcePath, file)
	if err != nil {
		return err
	}
	err = root.WriteExclusive(
		name,
		file.Size,
		func(w io.Writer) error { return safefs.CopyVerified(w, source, file.Size, file.SHA256) },
	)
	return errors.Join(err, source.Close())
}

func (m *Manager) checkRequirements(ctx context.Context, env model.Environment, tree string) error {
	if env.Baseline.FontLookup == "files" && len(env.Requires.Fonts) > 0 {
		return errors.New("this baseline permits filename font lookup only; named font requirements are unsupported")
	}
	for _, tool := range env.Requires.Tools {
		if env.Baseline.Toolchain[tool] == "" {
			return fmt.Errorf("baseline does not provide required tool %q", tool)
		}
	}
	fonts := append([]model.Font(nil), env.Baseline.Fonts...)
	if len(env.Requires.Fonts) > 0 {
		for _, resource := range env.Resources {
			if resource.Kind != "font" {
				continue
			}
			for _, file := range resource.Files {
				if !fontFilename(file.Path) {
					continue
				}
				faces, err := m.scanFont(ctx, env.Baseline, m.blobPath(file.SHA256), file)
				if err != nil {
					return fmt.Errorf("inspect font resource %q: %w", resource.ID, err)
				}
				fonts = append(fonts, faces...)
			}
		}
		if tree != "" {
			files, err := inventoryFiles(tree, m.cfg.MaxResourceBytes, m.cfg.MaxResourceFiles)
			if err != nil {
				return err
			}
			for _, file := range files {
				if !fontFilename(file.Path) {
					continue
				}
				faces, err := m.scanFont(ctx, env.Baseline, filepath.Join(tree, filepath.FromSlash(file.Path)), file)
				if err != nil {
					return err
				}
				fonts = append(fonts, faces...)
			}
		}
	}
	for _, requirement := range env.Requires.Fonts {
		found := false
		for _, font := range fonts {
			if fontMatches(font.Family, requirement.Family) &&
				(requirement.Style == "" || fontMatches(font.Style, requirement.Style)) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required font is unavailable: %s (%s)", requirement.Family, requirement.Style)
		}
	}
	return nil
}

func fontMatches(values, want string) bool {
	for _, value := range strings.Split(values, ",") {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// Lease pins an authorized, prepared environment until all source descriptors
// have been consumed. Release is idempotent and safe after cancellation.
type Lease struct {
	manager     *Manager
	record      *record
	environment model.Environment
	files       []MaterializedFile
	tree        string
	once        sync.Once
}

func (m *Manager) Pin(owner, digest string) (*Lease, error) {
	m.mu.Lock()
	rec, access, err := m.authorizeLocked(owner, digest)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	env := m.viewLocked(rec, access)
	if !env.Ready {
		m.mu.Unlock()
		return nil, ErrNotReady
	}
	if err = m.validateBaselineLocked(rec, env); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	lease := &Lease{manager: m, record: rec, environment: env}
	expectedTree := slices.Clone(rec.TreeFiles)
	rec.refs++
	m.mu.Unlock()
	verified := false
	defer func() {
		if !verified {
			lease.Release()
		}
	}()
	if err = m.verifyLease(lease, expectedTree); err != nil {
		return nil, err
	}

	// A concurrent resolve can replace access policy while the bytes are being
	// checked. Reauthorize before handing the verified lease to the caller.
	m.mu.Lock()
	_, access, err = m.authorizeLocked(owner, digest)
	if err == nil {
		env = m.viewLocked(rec, access)
		if !env.Ready {
			err = ErrNotReady
		} else {
			err = m.validateBaselineLocked(rec, env)
		}
	}
	if err == nil {
		lease.environment = env
		access.Used = time.Now().UTC()
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	verified = true
	return lease, nil
}

func (m *Manager) validateBaselineLocked(rec *record, env model.Environment) error {
	baseline, ok := m.baselines[env.Baseline.ID]
	if !ok || baseline.Digest != env.Baseline.Digest {
		return errors.New("locked baseline is no longer registered")
	}
	for _, inventory := range []model.Baseline{baseline, rec.Environment.Baseline, env.Baseline} {
		actual, err := model.BaselineDigest(inventory)
		if err != nil || actual != inventory.Digest || actual != baseline.Digest {
			return errors.New("locked baseline inventory failed integrity verification")
		}
	}
	return nil
}

func (m *Manager) verifyLease(lease *Lease, expectedTree []model.File) error {
	env := lease.environment
	filesByPath := make(map[string]int)
	verified := make(map[string]int64)
	for _, resource := range env.Resources {
		for _, file := range resource.Files {
			if size, ok := verified[file.SHA256]; ok {
				if size != file.Size {
					return errors.New("resource digest has conflicting sizes")
				}
			} else {
				f, err := verifiedOpen(m.blobPath(file.SHA256), file)
				if err != nil {
					return err
				}
				if err = f.Close(); err != nil {
					return err
				}
				verified[file.SHA256] = file.Size
			}
			if resource.Kind == "texmf" {
				continue
			}
			name, err := model.Destination(resource.Mount, file.Path)
			if err != nil {
				return err
			}
			if index, exists := filesByPath[name]; exists {
				previous := &lease.files[index]
				if previous.SHA256 != file.SHA256 || previous.Size != file.Size {
					return fmt.Errorf("resource destination collision: %s", name)
				}
				if resource.Kind == "font" {
					previous.Kind = "font"
				}
				continue
			}
			filesByPath[name] = len(lease.files)
			lease.files = append(
				lease.files,
				MaterializedFile{
					Path:   name,
					Source: m.blobPath(file.SHA256),
					SHA256: file.SHA256,
					Size:   file.Size,
					Kind:   resource.Kind,
				},
			)
		}
	}
	if needsTree(env) {
		lease.tree = m.treePath(env.Digest)
		actual, err := inventoryFiles(lease.tree, m.cfg.MaxResourceBytes, m.cfg.MaxResourceFiles)
		if err != nil {
			return err
		}
		if !slices.Equal(actual, expectedTree) {
			return errors.New("prepared TEXMF tree failed integrity verification")
		}
	}
	return nil
}

func (l *Lease) Environment() model.Environment { return copyEnvironment(l.environment) }
func (l *Lease) Files() []MaterializedFile      { return append([]MaterializedFile(nil), l.files...) }
func (l *Lease) Tree() string                   { return l.tree }
func (l *Lease) RunnerConfig(cfg config.Config) config.Config {
	resourceDefaults(&cfg)
	cfg.RunnerImage = l.environment.Baseline.Image
	cfg.Engines = append([]string(nil), l.environment.Baseline.Engines...)
	return cfg
}
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.manager.releaseRef(l.record) })
}
