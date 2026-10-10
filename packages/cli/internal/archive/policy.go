package archive

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/shared/safefs"
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

const maxPolicyBytes = 1 << 20

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
		if errors.Is(err, os.ErrNotExist) && opts.IgnoreFiles == nil {
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
	fs, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fs.Close() }()
	data, err := fs.ReadLimited(filepath.ToSlash(filepath.Clean(name)), maxPolicyBytes)
	if err != nil {
		return nil, fmt.Errorf("read policy %s: %w", name, err)
	}
	return data, nil
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
	if !p.nested {
		return false, "", nil
	}
	// Evaluate cached per-directory layers in parent-to-child order. Compiling a
	// combined matcher for every leaf repeatedly reads and duplicates root rules.
	matched := false
	var last *ignoreRule
	bases := []string{"."}
	dir := path.Dir(rel)
	if dir != "." {
		current := ""
		for _, part := range strings.Split(dir, "/") {
			current = path.Join(current, part)
			bases = append(bases, current)
		}
	}
	for _, base := range bases {
		matcher, err := p.nestedMatcher(base)
		if err != nil {
			return false, "", err
		}
		if valueMatched, rule := matcher.MatchesPathHow(value); rule != nil {
			matched, last = valueMatched, rule
		}
	}
	if matched {
		return true, last.Line, nil
	}
	return false, "", nil
}

func (p *filePolicy) nestedMatcher(base string) (*ignoreMatcher, error) {
	if matcher, exists := p.matchers[base]; exists {
		return matcher, nil
	}
	data, err := readPolicy(p.root, path.Join(base, ".gitignore"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	matcher := compileIgnoreLines(strings.Split(string(data), "\n")...)
	if base != "." {
		matcher.base = base
	}
	p.matchers[base] = matcher
	return matcher, nil
}
