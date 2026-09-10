package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/config"
)

type filePolicy struct {
	matchers map[string]*ignoreMatcher
	root     string
	patterns []string
	deny     *ignoreMatcher
	local    *ignoreMatcher
	files    map[string]bool
	nested   bool
}

func newPolicy(opts Options) (*filePolicy, error) {
	p := &filePolicy{
		root:     opts.Root,
		matchers: make(map[string]*ignoreMatcher),
		patterns: append([]string{}, opts.Exclude...),
		deny:     compileIgnoreLines(config.DefaultDeny()...),
		files:    make(map[string]bool),
		nested:   opts.RespectGitIgnore && !hasGitMarker(opts.Root),
	}
	for _, name := range opts.DenyFiles {
		if name == "" {
			continue
		}
		absolute, err := filepath.Abs(name)
		if err != nil {
			return nil, err
		}
		roots := []string{opts.Root}
		if resolved, err := filepath.EvalSymlinks(opts.Root); err == nil {
			roots = append(roots, resolved)
		}
		names := []string{absolute}
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			names = append(names, resolved)
		}
		for _, root := range roots {
			root, err = filepath.Abs(root)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				rel, err := filepath.Rel(root, name)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					p.files[filepath.ToSlash(rel)] = true
				}
			}
		}
	}
	names := opts.IgnoreFiles
	if names == nil {
		names = []string{".latexmkignore"}
	}
	for _, name := range names {
		data, err := readPolicy(opts.Root, name)
		if os.IsNotExist(err) && opts.IgnoreFiles == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		p.files[filepath.ToSlash(filepath.Clean(name))] = true
		p.patterns = append(p.patterns, strings.Split(string(data), "\n")...)
	}
	p.local = compileIgnoreLines(p.patterns...)
	return p, nil
}

func readPolicy(root, name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, fmt.Errorf("policy file must stay inside project root: %s", name)
	}
	current := root
	for _, part := range strings.Split(filepath.Clean(name), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("policy path contains symlink: %s", name)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("invalid or oversized policy file: %s", name)
	}
	return os.ReadFile(current)
}

func (p *filePolicy) excluded(rel string, directory bool) (bool, string, error) {
	value := rel
	if directory {
		value += "/"
	}
	if p.files[rel] || p.deny.MatchesPath(value) {
		return true, "local configuration or credential", nil
	}
	if matched, rule := p.local.MatchesPathHow(value); matched {
		return true, rule.Line, nil
	}
	dirKey := filepath.Dir(rel)
	if !p.nested {
		dirKey = "."
	}
	if matcher, exists := p.matchers[dirKey]; exists {
		matched, rule := matcher.MatchesPathHow(value)
		if matched && rule != nil {
			return true, rule.Line, nil
		}
		return false, "", nil
	}
	var patterns []string
	if p.nested {
		// Apply nested rules in parent-to-child order, preserving Git's parent pruning.
		dir := filepath.ToSlash(filepath.Dir(rel))
		bases := []string{"."}
		if dir != "." {
			current := ""
			for _, part := range strings.Split(dir, "/") {
				if current != "" {
					current += "/"
				}
				current += part
				bases = append(bases, current)
			}
		}
		for _, base := range bases {
			name := filepath.Join(base, ".gitignore")
			data, err := readPolicy(p.root, name)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return false, "", err
			}
			for _, line := range strings.Split(string(data), "\n") {
				if base == "." || line == "" || strings.HasPrefix(line, "#") {
					patterns = append(patterns, line)
					continue
				}
				prefix := ""
				if strings.HasPrefix(line, "!") {
					prefix = "!"
					line = line[1:]
				}
				anchored := strings.Contains(strings.TrimSuffix(line, "/"), "/")
				line = strings.TrimPrefix(line, "/")
				if !anchored {
					line = "**/" + line
				}
				patterns = append(patterns, prefix+"/"+base+"/"+line)
			}
		}
	}
	matcher := compileIgnoreLines(patterns...)
	p.matchers[dirKey] = matcher
	matched, rule := matcher.MatchesPathHow(value)
	if matched && rule != nil {
		return true, rule.Line, nil
	}
	return false, "", nil
}
