package dependency

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxManifestBytes = 1 << 20
	maxManifestFiles = 20_000
)

// LoadExplicitManifest reads an exact project-relative file list. Blank lines
// and lines whose first non-space character is # are ignored.
func LoadExplicitManifest(root, manifestPath string) ([]string, error) {
	if strings.TrimSpace(manifestPath) == "" {
		return nil, nil
	}
	clean, err := NormalizeExplicitManifestPath(manifestPath)
	if err != nil {
		return nil, err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rootAbs, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	abs := filepath.Join(rootAbs, filepath.FromSlash(clean))
	current := rootAbs
	for _, part := range strings.Split(filepath.FromSlash(clean), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("inspect manifest %s: %w", clean, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("manifest path contains a symbolic link: %s", clean)
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("inspect manifest %s: %w", clean, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest %s is not a regular file", clean)
	}
	if info.Size() > maxManifestBytes {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxManifestBytes)
	}
	payload, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", clean, err)
	}
	unique := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 64<<10), 256<<10)
	line := 0
	for scanner.Scan() {
		line++
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\uFEFF"))
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		path := cleanProjectPath(value)
		if path == "" {
			return nil, fmt.Errorf("manifest %s:%d contains an out-of-root path", clean, line)
		}
		unique[path] = struct{}{}
		if len(unique) > maxManifestFiles {
			return nil, fmt.Errorf("manifest contains more than %d files", maxManifestFiles)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", clean, err)
	}
	files := make([]string, 0, len(unique))
	for file := range unique {
		files = append(files, file)
	}
	sort.Strings(files)
	return files, nil
}

// NormalizeExplicitManifestPath returns an exact path that can also be added
// to archive excludes without being interpreted as a glob.
func NormalizeExplicitManifestPath(manifestPath string) (string, error) {
	clean := cleanProjectPath(manifestPath)
	if clean == "" {
		return "", errors.New("manifest path escapes the project root")
	}
	if strings.ContainsAny(clean, "*?[") {
		return "", errors.New("manifest path cannot contain glob characters")
	}
	return clean, nil
}
