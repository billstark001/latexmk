package archive

import (
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type ignoreRule struct {
	Line                        string
	pattern                     string
	negate, directory, anchored bool
}

type ignoreMatcher struct{ rules []ignoreRule }

func compileIgnoreLines(lines ...string) *ignoreMatcher {
	matcher := &ignoreMatcher{}
	for _, line := range lines {
		raw := strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(raw, " ") && !strings.HasSuffix(raw, "\\ ") {
			raw = strings.TrimSuffix(raw, " ")
		}
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		rule := ignoreRule{Line: line}
		if strings.HasPrefix(raw, "!") {
			rule.negate = true
			raw = raw[1:]
		}
		rule.directory = strings.HasSuffix(raw, "/")
		raw = strings.TrimSuffix(raw, "/")
		raw = strings.TrimPrefix(raw, "./")
		rule.anchored = strings.Contains(raw, "/")
		rule.pattern = strings.TrimPrefix(raw, "/")
		if rule.pattern != "" && doublestar.ValidatePattern(rule.pattern) {
			matcher.rules = append(matcher.rules, rule)
		}
	}
	return matcher
}

func (m *ignoreMatcher) MatchesPath(value string) bool {
	matched, _ := m.MatchesPathHow(value)
	return matched
}

// Parent directories are evaluated by the walker before their contents. A
// directory-only negation must not accidentally re-include all its descendants.
func (m *ignoreMatcher) MatchesPathHow(value string) (bool, *ignoreRule) {
	directory := strings.HasSuffix(value, "/")
	name := strings.TrimSuffix(value, "/")
	matched := false
	var last *ignoreRule
	for i := range m.rules {
		rule := &m.rules[i]
		if rule.directory && !directory {
			continue
		}
		candidate := name
		if !rule.anchored {
			candidate = path.Base(name)
		}
		if strings.HasSuffix(rule.pattern, "/**") && candidate == strings.TrimSuffix(rule.pattern, "/**") {
			continue
		}
		if doublestar.MatchUnvalidated(rule.pattern, candidate) {
			matched = !rule.negate
			last = rule
		}
	}
	return matched, last
}
