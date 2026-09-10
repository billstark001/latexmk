// Package dependency selects literal LaTeX dependencies from an already
// policy-filtered project manifest. It never reads outside that manifest.
package dependency

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

const maxParsedFileSize = 8 << 20

type Diagnostic struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Command    string `json:"command"`
	Reference  string `json:"reference,omitempty"`
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	Resolution string `json:"resolution,omitempty"`
}

type Result struct {
	Files       []projectarchive.File
	Stats       projectarchive.Stats
	Diagnostics []Diagnostic
	Resolved    bool
}

type SelectionOptions struct {
	Mode          string
	ExplicitFiles []string
	CachedFiles   []string
	UnmatchedGlob string
}

// SelectWithOptions combines static discovery, explicit files, and recorder
// history without letting any layer restore a file absent from candidates.
func SelectWithOptions(entry string, candidates []projectarchive.File, options SelectionOptions) (Result, error) {
	entry = cleanProjectPath(entry)
	if entry == "" {
		return Result{}, errors.New("entry path is outside the project root")
	}
	mode := options.Mode
	if mode == "" {
		mode = "auto"
	}
	if mode == "all" {
		files := append([]projectarchive.File(nil), candidates...)
		stats := projectarchive.Stats{}
		for i := range files {
			files[i].Reason = "upload mode all"
			stats.Files++
			stats.Bytes += files[i].Size
		}
		return Result{Files: files, Stats: stats, Resolved: true}, nil
	}
	if mode != "auto" && mode != "manifest" {
		return Result{}, fmt.Errorf("unsupported upload mode %q", mode)
	}

	byPath := make(map[string]projectarchive.File, len(candidates))
	for _, file := range candidates {
		byPath[file.Path] = file
	}
	if _, ok := byPath[entry]; !ok {
		return Result{}, fmt.Errorf("entry %q is missing, ignored, or denied by the upload policy", entry)
	}
	var result Result
	if mode == "auto" {
		var err error
		result, err = Discover(entry, candidates)
		if err != nil {
			return Result{}, err
		}
	} else {
		file := byPath[entry]
		file.Reason = "entry file"
		result = Result{Files: []projectarchive.File{file}, Resolved: true}
	}
	selected := make(
		map[string]projectarchive.File,
		len(result.Files)+len(options.ExplicitFiles)+len(options.CachedFiles),
	)
	for _, file := range result.Files {
		selected[file.Path] = file
	}
	for _, expression := range options.ExplicitFiles {
		pattern, err := NormalizePattern(expression)
		if err != nil {
			return Result{}, err
		}
		count := 0
		for _, file := range candidates {
			if !doublestar.MatchUnvalidated(pattern, file.Path) {
				continue
			}
			count++
			if _, exists := selected[file.Path]; !exists {
				file.Reason = "explicit manifest: " + expression
				selected[file.Path] = file
			}
		}
		if count == 0 {
			diagnostic := Diagnostic{
				File:      entry,
				Reference: expression,
				Kind:      "explicit",
				Message:   "pattern has no allowed matches (missing, ignored, or denied)",
			}
			if HasGlob(pattern) {
				switch options.UnmatchedGlob {
				case "ignore":
					continue
				case "warn":
					diagnostic.Resolution = "unmatchedGlob=warn"
				}
			}
			result.Diagnostics = append(result.Diagnostics, diagnostic)
		}
	}
	if mode == "auto" {
		for _, cachedPath := range options.CachedFiles {
			file, ok := byPath[cachedPath]
			if !ok {
				continue
			}
			if _, exists := selected[cachedPath]; exists {
				continue
			}
			file.Reason = "previous successful compile (.fls INPUT)"
			selected[cachedPath] = file
		}
	}
	result.Files = result.Files[:0]
	result.Stats = projectarchive.Stats{}
	for _, file := range selected {
		result.Files = append(result.Files, file)
		result.Stats.Files++
		result.Stats.Bytes += file.Size
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	result.Resolved = true
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Resolution == "" {
			result.Resolved = false
			break
		}
	}
	return result, nil
}

func FormatDiagnostic(diagnostic Diagnostic) string {
	location := diagnostic.File
	if diagnostic.Line > 0 {
		location = fmt.Sprintf("%s:%d", location, diagnostic.Line)
	}
	reference := diagnostic.Reference
	if diagnostic.Command != "" {
		reference = "\\" + diagnostic.Command + "{" + diagnostic.Reference + "}"
	}
	message := diagnostic.Message
	if diagnostic.Resolution != "" {
		message += "; covered by " + diagnostic.Resolution
	}
	if reference != "" {
		return fmt.Sprintf("%s: %s: %s", location, reference, message)
	}
	return fmt.Sprintf("%s: %s", location, message)
}

func balanced(text string, start int, open, close byte) (string, int, bool) {
	depth := 0
	for i := start; i < len(text); i++ {
		if text[i] == '\\' {
			i++ // Escaped delimiters are not grouping tokens.
			continue
		}
		if open == '[' && text[i] == '{' {
			_, next, ok := balanced(text, i, '{', '}')
			if !ok {
				return "", start, false
			}
			i = next - 1
			continue
		}
		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return text[start+1 : i], i + 1, true
			}
		}
	}
	return "", start, false
}

func skipSpace(text string, at int) int {
	for at < len(text) && (text[at] == 0 || text[at] == ' ' || text[at] == '\t' || text[at] == '\r' || text[at] == '\n') {
		at++
	}
	return at
}

func sanitize(text string) string {
	bytes := []byte(text)
	for i := 0; i < len(bytes); i++ {
		if bytes[i] != '%' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && bytes[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continue
		}
		for i < len(bytes) && bytes[i] != '\n' {
			bytes[i] = '\x00'
			i++
		}
		if i < len(bytes) {
			for j := i + 1; j < len(bytes) && (bytes[j] == ' ' || bytes[j] == '\t'); j++ {
				bytes[j] = '\x00'
			}
		}
	}
	text = string(bytes)
	text = maskInlineVerb(text, "\\lstinline")
	text = maskInlineVerb(text, "\\verb")
	return text
}

func maskInlineVerb(text, token string) string {
	for search := 0; ; {
		relative := strings.Index(text[search:], token)
		if relative < 0 {
			return text
		}
		start := search + relative
		cursor := start + len(token)
		if cursor < len(text) &&
			((text[cursor] >= 'A' && text[cursor] <= 'Z') || (text[cursor] >= 'a' && text[cursor] <= 'z')) {
			search = cursor
			continue
		}
		if cursor < len(text) && text[cursor] == '*' {
			cursor++
		}
		if token == "\\lstinline" && cursor < len(text) && text[cursor] == '[' {
			_, next, ok := balanced(text, cursor, '[', ']')
			if !ok {
				return text
			}
			cursor = next
		}
		if cursor >= len(text) || text[cursor] == '\n' || text[cursor] == '\r' {
			search = cursor
			continue
		}
		delimiter := text[cursor]
		endRelative := strings.IndexByte(text[cursor+1:], delimiter)
		if endRelative < 0 {
			return text
		}
		end := cursor + 1 + endRelative + 1
		masked := []byte(text)
		for i := start; i < end; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
		text = string(masked)
		search = end
	}
}

func bracedList(value string) ([]string, bool) {
	result := make([]string, 0)
	for cursor := skipSpace(value, 0); cursor < len(value); cursor = skipSpace(value, cursor) {
		if value[cursor] != '{' {
			return nil, false
		}
		item, next, ok := balanced(value, cursor, '{', '}')
		if !ok || !literalReference(item) {
			return nil, false
		}
		result = append(result, strings.TrimSpace(item))
		cursor = next
	}
	return result, len(result) > 0
}

func literalReference(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.ContainsAny(value, "\\#$%{}~")
}

func cleanProjectPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(strings.Split(value, "/")[0], ":") {
		return ""
	}
	clean := path.Clean(strings.TrimPrefix(value, "./"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// normalizeArgument removes TeX comment continuations without erasing literal spaces.
func normalizeArgument(value string) string {
	var out strings.Builder
	comment := false
	for _, c := range value {
		if c == 0 {
			comment = true
			continue
		}
		if c == '\n' && comment {
			continue
		}
		comment = false
		out.WriteRune(c)
	}
	return strings.TrimSpace(out.String())
}
