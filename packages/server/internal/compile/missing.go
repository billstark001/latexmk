package compile

import (
	"bytes"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const (
	maxMissingFiles   = 32
	maxMissingMatches = 4096
	maxMissingLogRead = 8 << 20
)

var missingFilePatterns = []*regexp.Regexp{
	regexp.MustCompile(
		"(?i)(?:latex error: file|package [^\\r\\n]* error: file)\\s+[`'\"]([^`'\"\\r\\n]+)[`'\"]\\s+not found",
	),
	regexp.MustCompile("(?i)i can't find file\\s+[`'\"]([^`'\"\\r\\n]+)[`'\"]"),
}

// detectMissingFiles extracts conservative, project-relative file requests
// from TeX diagnostics. The client remains responsible for upload policy.
func detectMissingFiles(stdout, stderr []byte, artifacts []File) []string {
	found := make(map[string]struct{})
	collect := func(source []byte) {
		source = source[:min(len(source), maxMissingLogRead)]
		for _, pattern := range missingFilePatterns {
			for _, match := range pattern.FindAllSubmatchIndex(source, maxMissingMatches) {
				if len(match) < 4 || match[2] < 0 {
					continue
				}
				if clean := cleanMissingPath(string(bytes.TrimSpace(source[match[2]:match[3]]))); clean != "" {
					found[clean] = struct{}{}
					if len(found) == maxMissingFiles {
						return
					}
				}
			}
		}
	}
	for _, source := range [][]byte{stdout, stderr} {
		if len(found) == maxMissingFiles {
			break
		}
		collect(source)
	}
	remaining := int64(maxMissingLogRead)
	for _, artifact := range artifacts {
		if remaining <= 0 || len(found) == maxMissingFiles {
			break
		}
		if !strings.HasSuffix(strings.ToLower(artifact.RelativePath), ".log") {
			continue
		}
		file, err := artifact.Open()
		if err != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(file, remaining))
		_ = file.Close()
		remaining -= int64(len(content))
		if readErr == nil {
			collect(content)
		}
	}

	result := make([]string, 0, len(found))
	for file := range found {
		result = append(result, file)
	}
	sort.Strings(result)
	return result
}

func cleanMissingPath(value string) string {
	if len(value) > 256 || strings.ContainsFunc(value, unicode.IsControl) {
		return ""
	}
	clean, err := safefs.Clean(value)
	if err != nil {
		return ""
	}
	return clean
}
