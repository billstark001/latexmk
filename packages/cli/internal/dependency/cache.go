package dependency

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/shared/safefs"
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
	if entry == "" || entry == "." {
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
	if entry == "" || entry == "." {
		return errors.New("cache entry path escapes the project root")
	}
	if !validCacheEngine(engine) {
		return errors.New("dependency cache contains an invalid engine")
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
	fs, err := safefs.Open(root)
	if err != nil {
		return cache, false, fmt.Errorf("open project root: %w", err)
	}
	defer func() { _ = fs.Close() }()
	payload, err := fs.ReadLimited(cacheDirName+"/"+cacheFileName, maxCacheBytes)
	if errors.Is(err, os.ErrNotExist) {
		return cache, false, nil
	}
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
		if clean := cleanProjectPath(item.Entry); clean == "" || clean == "." || clean != item.Entry {
			return cache, false, fmt.Errorf("dependency cache contains invalid entry path %q", item.Entry)
		}
		if !validCacheEngine(item.Engine) {
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
	payload, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if len(payload) > maxCacheBytes {
		return fmt.Errorf("dependency cache exceeds %d bytes", maxCacheBytes)
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return fmt.Errorf("open project root: %w", err)
	}
	defer func() { _ = fs.Close() }()
	_, err = fs.WriteAtomic(cacheDirName+"/"+cacheFileName, maxCacheBytes, func(writer io.Writer) error {
		_, err := writer.Write(payload)
		return err
	})
	return err
}

// Engine keys are opaque registry names. The complete cache size already bounds
// their length; an arbitrary 64-byte cap would break valid custom drivers.
func validCacheEngine(name string) bool {
	return strings.TrimSpace(name) != "" && !strings.ContainsAny(name, "\x00\r\n")
}

func normalizeCachedPaths(values []string) ([]string, error) {
	if len(values) > maxCacheItems {
		return nil, fmt.Errorf("dependency cache contains more than %d input paths", maxCacheItems)
	}
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		clean := cleanProjectPath(value)
		if _, err := safefs.Clean(clean); err != nil {
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
