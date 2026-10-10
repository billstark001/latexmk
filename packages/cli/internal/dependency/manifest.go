package dependency

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const (
	maxManifestBytes = 1 << 20
	maxManifestFiles = 20_000
)

var exactPatternEscaper = strings.NewReplacer(
	"\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]", "{", "\\{", "}", "\\}",
)

// ExactPattern quotes a validated filename when mixing it with user glob input.
// The returned pattern matches only that filename, including literal glob syntax.
func ExactPattern(name string) string { return exactPatternEscaper.Replace(name) }

// LoadExplicitManifest reads project-relative paths and glob patterns. Blank lines
// and lines whose first non-space character is # are ignored.
func LoadExplicitManifest(root, manifestPath string) ([]string, error) {
	if strings.TrimSpace(manifestPath) == "" {
		return nil, nil
	}
	clean, err := NormalizeExplicitManifestPath(manifestPath)
	if err != nil {
		return nil, err
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", err)
	}
	defer func() { _ = fs.Close() }()
	payload, err := fs.ReadLimited(clean, maxManifestBytes)
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
		path, patternErr := NormalizePattern(value)
		if patternErr != nil {
			return nil, fmt.Errorf("manifest %s:%d contains an invalid pattern: %w", clean, line, patternErr)
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

// NormalizePattern validates a slash-separated, root-relative pattern before matching.
func NormalizePattern(value string) (string, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "./"))
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, ":") {
		return "", fmt.Errorf("invalid project-relative pattern %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", fmt.Errorf("pattern escapes project root: %q", value)
		}
	}
	if !doublestar.ValidatePattern(value) {
		return "", fmt.Errorf("invalid glob %q", value)
	}
	if strings.HasSuffix(value, "/") {
		value += "**"
	}
	return value, nil
}

// HasGlob distinguishes patterns from escaped literal filenames, which must
// still fail when missing even if unmatchedGlob is warn or ignore.
func HasGlob(pattern string) bool {
	escaped := false
	for _, character := range pattern {
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if strings.ContainsRune("*?[{", character) {
			return true
		}
	}
	return false
}
