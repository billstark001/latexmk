package dependency

import (
	"fmt"
	"path"
	"sort"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

var requestedFileExtensions = []string{
	".tex", ".pdf", ".png", ".jpg", ".jpeg", ".eps", ".mps", ".svg",
	".bib", ".sty", ".cls", ".bst", ".dat", ".csv",
}

// ResolveRequestedFiles resolves server diagnostics only within an already
// policy-filtered candidate manifest. It never reads or restores other paths.
func ResolveRequestedFiles(requested []string, candidates []projectarchive.File) ([]projectarchive.File, error) {
	byPath := make(map[string]projectarchive.File, len(candidates))
	for _, file := range candidates {
		byPath[file.Path] = file
	}
	selected := make(map[string]projectarchive.File)
	for _, raw := range requested {
		clean := cleanProjectPath(raw)
		if clean == "" {
			return nil, fmt.Errorf("server requested an unsafe project path %q", raw)
		}
		if file, ok := byPath[clean]; ok {
			file.Reason = "validated server missing-file request"
			selected[file.Path] = file
			continue
		}
		if path.Ext(clean) != "" {
			return nil, fmt.Errorf("requested file %q is absent, ignored, or denied by the local upload policy", clean)
		}
		matches := make([]projectarchive.File, 0, 1)
		for _, extension := range requestedFileExtensions {
			if file, ok := byPath[clean+extension]; ok {
				matches = append(matches, file)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("requested file %q is absent, ignored, or denied by the local upload policy", clean)
		case 1:
			file := matches[0]
			file.Reason = "validated server missing-file request"
			selected[file.Path] = file
		default:
			paths := make([]string, 0, len(matches))
			for _, file := range matches {
				paths = append(paths, file.Path)
			}
			sort.Strings(paths)
			return nil, fmt.Errorf("requested file %q is ambiguous (%s); add the intended path to an explicit manifest", clean, strings.Join(paths, ", "))
		}
	}
	result := make([]projectarchive.File, 0, len(selected))
	for _, file := range selected {
		result = append(result, file)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
