package compile

import (
	"bytes"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	maxMissingFiles   = 32
	maxMissingLogRead = 8 << 20
)

var missingFilePatterns = []*regexp.Regexp{
	regexp.MustCompile("(?i)(?:latex error: file|package [^\\r\\n]* error: file)\\s+[`'\"]([^`'\"\\r\\n]+)[`'\"]\\s+not found"),
	regexp.MustCompile("(?i)i can't find file\\s+[`'\"]([^`'\"\\r\\n]+)[`'\"]"),
}

// detectMissingFiles extracts conservative, project-relative file requests
// from TeX diagnostics. The client remains responsible for upload policy.
func detectMissingFiles(stdout, stderr []byte, artifacts []File) []string {
	var sources [][]byte
	sources = append(sources, stdout, stderr)
	remaining := int64(maxMissingLogRead)
	for _, artifact := range artifacts {
		if remaining <= 0 || !strings.HasSuffix(strings.ToLower(artifact.RelativePath), ".log") {
			continue
		}
		f, err := os.Open(artifact.AbsolutePath)
		if err != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(f, remaining))
		_ = f.Close()
		if readErr != nil {
			continue
		}
		remaining -= int64(len(content))
		sources = append(sources, content)
	}

	found := make(map[string]struct{})
	for _, source := range sources {
		for _, pattern := range missingFilePatterns {
			for _, match := range pattern.FindAllSubmatch(source, -1) {
				if len(match) < 2 {
					continue
				}
				if clean := cleanMissingPath(string(bytes.TrimSpace(match[1]))); clean != "" {
					found[clean] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(found))
	for file := range found {
		result = append(result, file)
	}
	sort.Strings(result)
	if len(result) > maxMissingFiles {
		result = result[:maxMissingFiles]
	}
	return result
}

func cleanMissingPath(value string) string {
	if value == "" || len(value) > 256 || strings.Contains(value, "\\") || path.IsAbs(value) {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}
