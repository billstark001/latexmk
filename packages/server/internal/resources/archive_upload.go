package resources

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"slices"
	"time"

	model "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

var (
	ErrArchiveChecksum = errors.New("uploaded TeX Live archive has a checksum mismatch")
	ErrArchiveSize     = errors.New("uploaded TeX Live archive exceeds its byte limit or declared length")
)

// PackageArchives exposes transport policy and cache availability without
// modifying the environment manifest or accepting client-provided package plans.
func (m *Manager) PackageArchives(ctx context.Context, owner, digest string) (model.PackageArchives, error) {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ResourcePrepareTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return model.PackageArchives{}, err
	}
	m.mu.Lock()
	rec, access, err := m.authorizeLocked(owner, digest)
	if err != nil {
		m.mu.Unlock()
		return model.PackageArchives{}, err
	}
	rec.refs++
	access.Used = time.Now().UTC()
	packages := slices.Clone(access.Environment.Packages)
	m.mu.Unlock()
	defer m.releaseRef(rec)
	// Presence checks verify cached bytes. Share the preparation admission limit
	// so repeated manifest requests cannot start unbounded archive hashing.
	select {
	case m.prepares <- struct{}{}:
		defer func() { <-m.prepares }()
	case <-ctx.Done():
		return model.PackageArchives{}, ctx.Err()
	}
	manifest := model.PackageArchives{
		EnvironmentDigest: digest,
		Archives:          make([]model.PackageArchive, 0, len(packages)),
	}
	if policy := m.cfg.ResourceSourcePolicy; policy != nil {
		// PackageIndex is a server-local descriptor, never a public download path.
		manifest.Sources = &model.SourcePolicy{
			Version:          policy.Version,
			ArchiveMirrors:   slices.Clone(policy.ArchiveMirrors),
			PerSourceTimeout: policy.PerSourceTimeout,
		}
	}
	for _, pkg := range packages {
		present, err := packageArchivePresent(
			ctx,
			filepath.Join(m.dir, "archives"),
			pkg.Checksum,
			m.cfg.MaxResourceBytes,
		)
		if err != nil {
			return model.PackageArchives{}, err
		}
		manifest.Archives = append(
			manifest.Archives,
			model.PackageArchive{Package: pkg, Present: present, MaxBytes: m.cfg.MaxResourceBytes},
		)
	}
	return manifest, nil
}

func packageArchivePresent(ctx context.Context, directory, checksum string, limit int64) (bool, error) {
	if !validArchiveChecksum(checksum) || limit <= 0 {
		return false, errors.New("invalid package archive cache identity or limit")
	}
	root, err := safefs.Open(directory)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenRegular(checksum + ".tar.xz")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	hash := sha512.New()
	size, err := io.Copy(hash, io.LimitReader(safefs.WithContext(ctx, file), limit+1))
	if err != nil {
		return false, err
	}
	return size <= limit && hex.EncodeToString(hash.Sum(nil)) == checksum, nil
}

// PutPackageArchive only admits bytes selected by the authoritative resolver.
// expectedSize is the HTTP Content-Length, or -1 for a streaming request. Even
// idempotent requests verify their body, so a cached archive cannot hide a bad
// checksum or an overlong upload.
func (m *Manager) PutPackageArchive(
	ctx context.Context,
	owner, digest, checksum string,
	expectedSize int64,
	input io.Reader,
) error {
	if !validArchiveChecksum(checksum) {
		return ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.ResourcePrepareTimeout)
	defer cancel()
	m.mu.Lock()
	rec, access, err := m.authorizeLocked(owner, digest)
	if err == nil &&
		!slices.ContainsFunc(
			access.Environment.Packages,
			func(pkg model.Package) bool { return pkg.Checksum == checksum },
		) {
		err = ErrNotFound
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	rec.refs++
	m.mu.Unlock()
	defer m.releaseRef(rec)
	if expectedSize < -1 || expectedSize > m.cfg.MaxResourceBytes {
		return ErrArchiveSize
	}
	select {
	case m.prepares <- struct{}{}:
		defer func() { <-m.prepares }()
	case <-ctx.Done():
		return ctx.Err()
	}
	directory := filepath.Join(m.dir, "archives")
	release, err := acquirePackageArchive(ctx, directory, checksum)
	if err != nil {
		return err
	}
	defer release()
	present, err := packageArchivePresent(ctx, directory, checksum, m.cfg.MaxResourceBytes)
	if err != nil {
		return err
	}
	limit := m.cfg.MaxResourceBytes
	if expectedSize >= 0 {
		limit = expectedSize
	}
	verify := func(destination io.Writer) error {
		hash := sha512.New()
		size, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(safefs.WithContext(ctx, input), limit+1))
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return context.DeadlineExceeded
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, safefs.ErrLimit) {
				return ErrArchiveSize
			}
			return err
		}
		if size > limit || (expectedSize >= 0 && size != expectedSize) {
			return ErrArchiveSize
		}
		if hex.EncodeToString(hash.Sum(nil)) != checksum {
			return ErrArchiveChecksum
		}
		return ctx.Err()
	}
	if present {
		return verify(io.Discard)
	}
	m.mu.Lock()
	err = m.reserveLocked(limit)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() { m.mu.Lock(); m.reserved -= limit; m.mu.Unlock() }()
	root, err := safefs.Open(directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	pending, err := root.Stage(checksum+".tar.xz", limit, verify)
	if err != nil {
		return err
	}
	defer func() { _ = pending.Close() }()
	if err := pending.Chmod(0444); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, access, err = m.authorizeLocked(owner, digest); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pending.Commit(); err != nil {
		return err
	}
	access.Used = time.Now().UTC()
	return nil
}
