package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Explanation struct {
	Path    string `json:"path"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

// Explain checks metadata and rules only; excluded file contents are never read.
func Explain(opts Options, name string) (Explanation, error) {
	result := Explanation{Path: name}
	if !filepath.IsLocal(name) {
		return result, fmt.Errorf("explanation path must stay inside project root")
	}
	policy, err := newPolicy(opts)
	if err != nil {
		return result, err
	}
	clean := filepath.Clean(name)
	parts := strings.Split(clean, string(filepath.Separator))
	for i := range parts {
		rel := filepath.Join(parts[:i+1]...)
		info, err := os.Lstat(filepath.Join(opts.Root, rel))
		directory := i < len(parts)-1 || (err == nil && info.IsDir())
		denied, reason, policyErr := policy.excluded(filepath.ToSlash(rel), directory)
		if policyErr != nil {
			return result, policyErr
		}
		if denied {
			result.Reason = "excluded by " + reason
			return result, nil
		}
		if os.IsNotExist(err) {
			result.Reason = "missing"
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			result.Reason = "symbolic link"
			return result, nil
		}
	}
	selection, err := loadGitSelection(opts.Root, opts.RespectGitIgnore)
	if err != nil {
		return result, err
	}
	if selection.Enabled {
		if _, ok := selection.Files[filepath.ToSlash(clean)]; !ok {
			result.Reason = "excluded by Git ignore rules"
			return result, nil
		}
	}
	result.Allowed, result.Reason = true, "allowed by upload policy"
	return result, nil
}
