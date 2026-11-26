// Package resourcecache stores verified public package archives outside projects.
package resourcecache

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const DefaultMaxBytes int64 = 512 << 20

type Settings struct {
	Directory   string `json:"directory,omitempty"`
	MaxBytes    int64  `json:"maxBytes,omitempty"`
	Acquisition string `json:"acquisition,omitempty"`
}

func (s Settings) Validate() error {
	if s.MaxBytes < 0 || s.MaxBytes > 1<<40 {
		return errors.New("resourceCache.maxBytes must be between 1 and 1099511627776, or omitted")
	}
	if s.Directory != "" && !filepath.IsAbs(s.Directory) {
		return errors.New("resource cache directory must be absolute")
	}
	switch s.Acquisition {
	case "", "auto", "client", "server":
		return nil
	default:
		return errors.New("resourceCache.acquisition must be auto, client or server")
	}
}

// Directory resolves existing ancestors without creating a cache. A cache may
// live inside a project (and must then be excluded), but cannot contain it.
func Directory(name, project string) (string, error) {
	if name == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		name = filepath.Join(base, "latexmk", "resources", "v1")
	}
	if !filepath.IsAbs(name) {
		return "", errors.New("resource cache directory must be absolute")
	}
	canonical, err := canonicalPath(name)
	if err != nil {
		return "", err
	}
	if project != "" {
		root, err := canonicalPath(project)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(canonical, root)
		if err != nil {
			return "", err
		}
		if rel == "." ||
			(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
			return "", errors.New("resource cache directory cannot equal or contain the project root")
		}
	}
	return canonical, nil
}

func canonicalPath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var missing []string
	probe := name
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

type Cache struct {
	directory string
	maxBytes  int64
	root      *safefs.Root
}

type Archive struct {
	File *os.File
	Size int64
	lock *safefs.FileLock
}

func (a *Archive) Close() error { return errors.Join(a.File.Close(), a.lock.Close()) }

func Open(settings Settings) (*Cache, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	directory, err := Directory(settings.Directory, "")
	if err != nil {
		return nil, err
	}
	limit := settings.MaxBytes
	if limit == 0 {
		limit = DefaultMaxBytes
	}
	if limit < 1 || limit > 1<<40 {
		return nil, errors.New("resource cache maxBytes must be between 1 and 1099511627776")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	root, err := safefs.Open(directory)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"archives", "locks"} {
		if err := root.MakeDirs(dir, 0700); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return &Cache{directory: directory, maxBytes: limit, root: root}, nil
}

func (c *Cache) Close() error { return c.root.Close() }

func validChecksum(value string) bool {
	if len(value) != sha512.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (c *Cache) lock(ctx context.Context, name string) (*safefs.FileLock, error) {
	for {
		lock, err := c.root.TryLock(name)
		if !errors.Is(err, safefs.ErrLocked) {
			return lock, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Acquire holds the archive lock until the caller finishes uploading. Failed
// sources are discarded by Stage; a verified object is published atomically.
func (c *Cache) Acquire(ctx context.Context, options resources.ArchiveFetchOptions) (_ *Archive, err error) {
	checksum := options.Package.Checksum
	if !validChecksum(checksum) || options.MaxBytes < 1 {
		return nil, errors.New("invalid package archive identity or limit")
	}
	if options.MaxBytes > c.maxBytes {
		options.MaxBytes = c.maxBytes
	}
	lock, err := c.lock(ctx, "locks/"+checksum+".lock")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	name := "archives/" + checksum + ".tar.xz"
	if file, size, readErr := c.verified(ctx, name, checksum, options.MaxBytes); readErr == nil {
		now := time.Now()
		_ = c.root.Chtimes(name, now, now)
		return &Archive{File: file, Size: size, lock: lock}, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		if !errors.Is(readErr, errCorrupt) {
			return nil, readErr
		}
		if err := c.root.Remove(name); err != nil {
			return nil, err
		}
	}
	budget, err := c.lock(ctx, "locks/budget.lock")
	if err != nil {
		return nil, err
	}
	defer func() { _ = budget.Close() }()
	// Only one cache miss stages bytes at a time. At most one bounded archive
	// can temporarily exist alongside the published cache budget.
	entries, err := c.root.ReadDir("archives")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if safefs.IsStagedName(entry.Name()) {
			if err := c.root.Remove("archives/" + entry.Name()); err != nil {
				return nil, err
			}
		}
	}
	_, err = resources.FetchArchive(ctx, options, func(write func(io.Writer) error) error {
		pending, err := c.root.Stage(name, options.MaxBytes, write)
		if err != nil {
			return err
		}
		defer func() { _ = pending.Close() }()
		if err := c.pruneLocked(pending.Size, checksum, true); err != nil {
			return err
		}
		if err := pending.Chmod(0600); err != nil {
			return err
		}
		return pending.Commit()
	})
	if err != nil {
		return nil, err
	}
	file, size, err := c.verified(ctx, name, checksum, options.MaxBytes)
	if err != nil {
		return nil, err
	}
	return &Archive{File: file, Size: size, lock: lock}, nil
}

var errCorrupt = errors.New("cached archive does not match its locked identity")

func (c *Cache) verified(ctx context.Context, name, checksum string, maxBytes int64) (*os.File, int64, error) {
	file, err := c.root.OpenRegular(name)
	if err != nil {
		return nil, 0, err
	}
	hash := sha512.New()
	size, err := io.Copy(hash, io.LimitReader(safefs.WithContext(ctx, file), maxBytes+1))
	if err == nil {
		if size > maxBytes {
			err = &resources.AcquisitionError{
				Code:   "resource_limit_exceeded",
				Stage:  "archive-cache",
				Reason: "bytes",
				Bytes:  size,
				Limit:  maxBytes,
			}
		} else if hex.EncodeToString(hash.Sum(nil)) != checksum {
			err = errCorrupt
		}
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, 0, errors.Join(err, file.Close())
	}
	return file, size, nil
}

type Info struct {
	Directory string `json:"directory"`
	Bytes     int64  `json:"bytes"`
	Archives  int    `json:"archives"`
	MaxBytes  int64  `json:"maxBytes"`
}

type cachedFile struct {
	name, checksum string
	size           int64
	modified       time.Time
}

func (c *Cache) inventory() ([]cachedFile, int64, error) {
	entries, err := c.root.ReadDir("archives")
	if err != nil {
		return nil, 0, err
	}
	files := make([]cachedFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		checksum := strings.TrimSuffix(entry.Name(), ".tar.xz")
		if !validChecksum(checksum) || entry.Name() != checksum+".tar.xz" {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() {
			return nil, 0, errors.New("resource cache contains a non-regular archive")
		}
		if info.Size() > 1<<40-total {
			return nil, 0, errors.New("resource cache accounting exceeds its limit")
		}
		total += info.Size()
		files = append(files, cachedFile{"archives/" + entry.Name(), checksum, info.Size(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	return files, total, nil
}

func (c *Cache) Info() (Info, error) {
	files, total, err := c.inventory()
	return Info{c.directory, total, len(files), c.maxBytes}, err
}

// Prune removes unused archives, skipping files leased by active processes.
// Locks remain in place to preserve the coordinating inode.
func (c *Cache) Prune(ctx context.Context, all bool) (Info, error) {
	lock, err := c.lock(ctx, "locks/budget.lock")
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = lock.Close() }()
	reserve := int64(0)
	if all {
		reserve = c.maxBytes
	}
	if err := c.pruneLocked(reserve, "", false); err != nil {
		return Info{}, err
	}
	return c.Info()
}

func (c *Cache) pruneLocked(reserve int64, held string, requireSpace bool) error {
	files, total, err := c.inventory()
	if err != nil {
		return err
	}
	for _, file := range files {
		if total <= c.maxBytes-reserve {
			break
		}
		if file.checksum == held {
			continue
		}
		lock, err := c.root.TryLock("locks/" + file.checksum + ".lock")
		if errors.Is(err, safefs.ErrLocked) {
			continue
		}
		if err != nil {
			return err
		}
		err = c.root.Remove(file.name)
		closeErr := lock.Close()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
		total -= file.size
	}
	if requireSpace && total > c.maxBytes-reserve {
		return &resources.AcquisitionError{
			Code:      "resource_cache_unavailable",
			Stage:     "archive-cache",
			Reason:    "capacity",
			Retryable: true,
			Bytes:     total,
			Limit:     c.maxBytes,
		}
	}
	return nil
}
