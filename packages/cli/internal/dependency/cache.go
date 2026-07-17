package dependency

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

const (
	cacheVersion  = 1
	cacheDirName  = ".latexmk-cache"
	cacheFileName = "dependencies.json"
	maxCacheBytes = 1 << 20
	maxCacheItems = 20_000
	maxCacheKeys  = 64
)

type cacheFile struct {
	Version int          `json:"version"`
	Entries []cacheEntry `json:"entries"`
}

type cacheEntry struct {
	Entry      string    `json:"entry"`
	Engine     string    `json:"engine"`
	InputFiles []string  `json:"inputFiles"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// LoadCachedInputs reads paths recorded by a previous successful compile.
// The cache contains project-relative paths only and never grants access to a
// file absent from the current policy-filtered manifest.
func LoadCachedInputs(root, entry, engine string) ([]string, bool, error) {
	entry = cleanProjectPath(entry)
	if entry == "" {
		return nil, false, errors.New("cache entry path escapes the project root")
	}
	cache, found, err := readCache(root)
	if err != nil || !found {
		return nil, false, err
	}
	for _, item := range cache.Entries {
		if item.Entry != entry || item.Engine != engine {
			continue
		}
		paths, err := normalizeCachedPaths(item.InputFiles)
		if err != nil {
			return nil, false, err
		}
		return paths, true, nil
	}
	return nil, false, nil
}

// SaveCachedInputs atomically stores workspace-local INPUT records from a
// successful remote compile.
func SaveCachedInputs(root, entry, engine string, inputFiles []string) error {
	entry = cleanProjectPath(entry)
	if entry == "" {
		return errors.New("cache entry path escapes the project root")
	}
	paths, err := normalizeCachedPaths(inputFiles)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("server returned no project input files")
	}
	cache, found, err := readCache(root)
	if err != nil {
		return err
	}
	if !found {
		cache = cacheFile{Version: cacheVersion}
	}
	next := make([]cacheEntry, 0, len(cache.Entries)+1)
	for _, item := range cache.Entries {
		if item.Entry == entry && item.Engine == engine {
			continue
		}
		next = append(next, item)
	}
	next = append(next, cacheEntry{Entry: entry, Engine: engine, InputFiles: paths, UpdatedAt: time.Now().UTC()})
	if len(next) > maxCacheKeys {
		sort.Slice(next, func(i, j int) bool { return next[i].UpdatedAt.After(next[j].UpdatedAt) })
		next = next[:maxCacheKeys]
	}
	sort.Slice(next, func(i, j int) bool {
		if next[i].Entry != next[j].Entry {
			return next[i].Entry < next[j].Entry
		}
		return next[i].Engine < next[j].Engine
	})
	cache.Version = cacheVersion
	cache.Entries = next
	return writeCache(root, cache)
}

func readCache(root string) (cacheFile, bool, error) {
	var cache cacheFile
	cacheDir := filepath.Join(root, cacheDirName)
	dirInfo, err := os.Lstat(cacheDir)
	if os.IsNotExist(err) {
		return cache, false, nil
	}
	if err != nil {
		return cache, false, fmt.Errorf("inspect dependency cache directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return cache, false, errors.New("dependency cache directory is not a real directory")
	}
	cachePath := filepath.Join(cacheDir, cacheFileName)
	info, err := os.Lstat(cachePath)
	if os.IsNotExist(err) {
		return cache, false, nil
	}
	if err != nil {
		return cache, false, fmt.Errorf("inspect dependency cache: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return cache, false, errors.New("dependency cache is not a regular file")
	}
	if info.Size() > maxCacheBytes {
		return cache, false, fmt.Errorf("dependency cache exceeds %d bytes", maxCacheBytes)
	}
	payload, err := os.ReadFile(cachePath)
	if err != nil {
		return cache, false, fmt.Errorf("read dependency cache: %w", err)
	}
	if err := json.Unmarshal(payload, &cache); err != nil {
		return cache, false, fmt.Errorf("parse dependency cache: %w", err)
	}
	if cache.Version != cacheVersion {
		return cache, false, fmt.Errorf("unsupported dependency cache version %d", cache.Version)
	}
	if len(cache.Entries) > maxCacheKeys {
		return cache, false, errors.New("dependency cache contains too many entries")
	}
	for i := range cache.Entries {
		item := &cache.Entries[i]
		if cleanProjectPath(item.Entry) != item.Entry {
			return cache, false, fmt.Errorf("dependency cache contains invalid entry path %q", item.Entry)
		}
		if item.Engine == "" || len(item.Engine) > 64 {
			return cache, false, errors.New("dependency cache contains an invalid engine")
		}
		paths, err := normalizeCachedPaths(item.InputFiles)
		if err != nil {
			return cache, false, err
		}
		item.InputFiles = paths
	}
	return cache, true, nil
}

func writeCache(root string, cache cacheFile) error {
	cacheDir := filepath.Join(root, cacheDirName)
	info, err := os.Lstat(cacheDir)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(cacheDir, 0o700); err != nil {
			return fmt.Errorf("create dependency cache directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect dependency cache directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return errors.New("dependency cache directory is not a real directory")
	}
	payload, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > maxCacheBytes {
		return fmt.Errorf("dependency cache exceeds %d bytes", maxCacheBytes)
	}
	tmp, err := os.CreateTemp(cacheDir, ".dependencies-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cachePath := filepath.Join(cacheDir, cacheFileName)
	if existing, err := os.Lstat(cachePath); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return errors.New("dependency cache target is not a regular file")
		}
		if runtime.GOOS == "windows" {
			if err := os.Remove(cachePath); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		return err
	}
	return nil
}

func normalizeCachedPaths(values []string) ([]string, error) {
	if len(values) > maxCacheItems {
		return nil, fmt.Errorf("dependency cache contains more than %d input paths", maxCacheItems)
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		clean := cleanProjectPath(value)
		if clean == "" {
			return nil, fmt.Errorf("invalid cached dependency path %q", value)
		}
		unique[clean] = struct{}{}
	}
	paths := make([]string, 0, len(unique))
	for value := range unique {
		paths = append(paths, value)
	}
	sort.Strings(paths)
	return paths, nil
}
