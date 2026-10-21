// Package naming implements a bounded, typed placeholder language over
// fasttemplate. It has no knowledge of CLI options, Git, paths or the clock.
// Callers supply values captured at the appropriate point in their workflow.
package naming

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/valyala/fasttemplate"
)

const MaxBytes = 64 << 10

// Value formats an already captured value. Implementations should be pure:
// repeated occurrences of a variable must not read changing external state.
type Value interface {
	Format(spec string) (string, error)
}

type Values map[string]Value

type placeholder struct{ name, spec string }

// Template is immutable after Parse and can be rendered concurrently.
type Template struct {
	compiled *fasttemplate.Template
	fields   []placeholder
	names    []string
}

// Parse accepts {variable[:format]}, with {{ and }} denoting literal braces.
// The format is opaque to the parser and may itself contain colons.
func Parse(source string) (*Template, error) {
	if len(source) > MaxBytes || !utf8.ValidString(source) || strings.ContainsRune(source, 0) {
		return nil, errors.New("template must be valid UTF-8 without NUL, at most 64 KiB")
	}
	t := &Template{}
	var compiled strings.Builder
	for i := 0; i < len(source); {
		switch source[i] {
		case '{':
			if i+1 < len(source) && source[i+1] == '{' {
				compiled.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(source[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unclosed placeholder at byte %d", i)
			}
			end += i + 1
			body := source[i+1 : end]
			name, spec, formatted := strings.Cut(body, ":")
			if !validName(name) || strings.ContainsRune(spec, '{') || (formatted && spec == "") {
				return nil, fmt.Errorf("invalid placeholder at byte %d", i)
			}
			// NUL-delimited internal tags cannot collide with user literals,
			// including escaped braces or values containing more placeholders.
			compiled.WriteString("\x00{" + strconv.Itoa(len(t.fields)) + "}\x00")
			t.fields = append(t.fields, placeholder{name, spec})
			if !slices.Contains(t.names, name) {
				t.names = append(t.names, name)
			}
			i = end + 1
		case '}':
			if i+1 >= len(source) || source[i+1] != '}' {
				return nil, fmt.Errorf("unescaped closing brace at byte %d", i)
			}
			compiled.WriteByte('}')
			i += 2
		default:
			compiled.WriteByte(source[i])
			i++
		}
	}
	var err error
	t.compiled, err = fasttemplate.NewTemplate(compiled.String(), "\x00{", "}\x00")
	return t, err
}

func validName(name string) bool {
	for _, part := range strings.Split(name, ".") {
		if part == "" {
			return false
		}
		for i, c := range part {
			if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') &&
				!(i > 0 && c >= '0' && c <= '9') {
				return false
			}
		}
	}
	return true
}

// Variables returns each dependency once, in first-occurrence order.
func (t *Template) Variables() []string { return slices.Clone(t.names) }

func (t *Template) Render(values Values) (string, error) {
	output := &boundedWriter{}
	_, err := t.compiled.ExecuteFunc(output, func(w io.Writer, tag string) (int, error) {
		index, err := strconv.Atoi(tag)
		if err != nil || index < 0 || index >= len(t.fields) {
			return 0, errors.New("invalid internal template tag")
		}
		field := t.fields[index]
		value, ok := values[field.name]
		if !ok || value == nil {
			return 0, fmt.Errorf("template variable %q is unavailable", field.name)
		}
		formatted, err := value.Format(field.spec)
		if err != nil {
			return 0, fmt.Errorf("template variable %q: %w", field.name, err)
		}
		return io.WriteString(w, formatted)
	})
	if err != nil {
		return "", err
	}
	return output.String(), nil
}

type boundedWriter struct{ strings.Builder }

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > MaxBytes-w.Len() {
		return 0, errors.New("rendered template exceeds 64 KiB")
	}
	return w.Builder.Write(p)
}

func (w *boundedWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// String accepts no format specifier.
type String string

func (v String) Format(spec string) (string, error) {
	if spec != "" {
		return "", errors.New("string values do not accept a format")
	}
	return string(v), nil
}

// Hash accepts an optional positive decimal prefix length. Prefixes are for
// display and do not claim uniqueness within any particular repository.
type Hash string

func (v Hash) Format(spec string) (string, error) {
	if len(v) == 0 {
		return "", errors.New("hash is empty")
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
			return "", errors.New("hash must be hexadecimal")
		}
	}
	if spec == "" {
		return string(v), nil
	}
	for _, c := range spec {
		if c < '0' || c > '9' {
			return "", errors.New("hash format must be a positive decimal prefix length")
		}
	}
	n, err := strconv.Atoi(spec)
	if err != nil || n < 1 || n > len(v) {
		return "", fmt.Errorf("hash prefix length must be between 1 and %d", len(v))
	}
	return string(v[:n]), nil
}

// Time represents a captured instant and its default Go layout. All named
// presets override that default; custom layouts require the explicit go: prefix.
type Time struct {
	Time   time.Time
	Layout string
}

func (v Time) Format(spec string) (string, error) {
	layout := v.Layout
	switch spec {
	case "":
		if layout == "" {
			layout = "20060102T150405"
		}
	case "date":
		layout = time.DateOnly
	case "time":
		layout = "15-04-05"
	case "compact":
		layout = "20060102T150405"
	case "iso8601", "rfc3339":
		layout = time.RFC3339
	case "rfc3339nano":
		layout = time.RFC3339Nano
	default:
		var ok bool
		layout, ok = strings.CutPrefix(spec, "go:")
		if !ok || layout == "" {
			return "", errors.New("unknown time format; use a named preset or go:LAYOUT")
		}
	}
	return v.Time.Format(layout), nil
}
