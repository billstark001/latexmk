package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValueSource declares exactly one literal, environment variable, or file.
type ValueSource struct {
	Value *string `json:"value,omitempty"`
	Env   string  `json:"env,omitempty"`
	File  string  `json:"file,omitempty"`
}

// LiteralSource constructs a literal value source, including an empty value.
func LiteralSource(value string) ValueSource {
	return ValueSource{Value: &value}
}

var errEmptySource = errors.New("source resolved to an empty value")
var errUnsetSource = errors.New("environment variable is unset")

// ServerSources stores all supported JSON forms as an ordered list of objects.
type ServerSources []ValueSource

func (s *ServerSources) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	items := []json.RawMessage{raw}
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return errors.New("server must be a string, source object, or array of strings/source objects")
		}
		if len(items) == 0 {
			return errors.New("server source array must not be empty")
		}
	}
	normalized := make(ServerSources, 0, len(items))
	for index, item := range items {
		source, err := parseValueSource(item, "server", "")
		if err != nil {
			return fmt.Errorf("server source %d: %w", index+1, err)
		}
		normalized = append(normalized, *source)
	}
	*s = normalized
	return nil
}

// MarshalJSON always emits the normalized object-array form.
func (s ServerSources) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return nil, errors.New("server source array must not be empty")
	}
	for index, source := range s {
		count := 0
		if source.Value != nil {
			count++
		}
		if source.Env != "" {
			count++
		}
		if source.File != "" {
			count++
		}
		if count != 1 || (source.Env != "" && strings.TrimSpace(source.Env) == "") ||
			(source.File != "" && strings.TrimSpace(source.File) == "") {
			return nil, fmt.Errorf("server source %d must declare exactly one valid source", index+1)
		}
	}
	return json.Marshal([]ValueSource(s))
}

func (s ServerSources) resolve(lookup func(string) (string, bool)) (string, error) {
	var unavailable []error
	for index := range s {
		value, err := s[index].resolve("server", lookup)
		if err == nil {
			return value, nil
		}
		err = fmt.Errorf("server source %d: %w", index+1, err)
		if !errors.Is(err, errEmptySource) && !errors.Is(err, errUnsetSource) && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		unavailable = append(unavailable, err)
	}
	return "", fmt.Errorf("server has no nonempty available source: %w", errors.Join(unavailable...))
}

func parseValueSource(raw json.RawMessage, field, base string) (*ValueSource, error) {
	raw = bytes.TrimSpace(raw)
	var literal string
	if string(raw) != "null" && json.Unmarshal(raw, &literal) == nil {
		if field == "token" {
			return nil, fmt.Errorf("token must use a file or environment source; hardcoded credentials are prohibited")
		}
		source := LiteralSource(literal)
		return &source, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != 1 {
		return nil, fmt.Errorf(
			"%s must declare exactly one source: value, env, or file (token allows only env or file)",
			field,
		)
	}
	source := &ValueSource{}
	for key, rawValue := range object {
		if field == "token" && key == "value" {
			return nil, fmt.Errorf("token must use a file or environment source; hardcoded credentials are prohibited")
		}
		var value string
		if string(rawValue) == "null" || json.Unmarshal(rawValue, &value) != nil ||
			(key != "value" && strings.TrimSpace(value) == "") {
			return nil, fmt.Errorf("%s source must be a nonempty string", field)
		}
		switch key {
		case "value":
			source.Value = &value
		case "env":
			source.Env = value
		case "file":
			if base != "" && !filepath.IsAbs(value) {
				value = filepath.Join(base, value)
			}
			source.File = value
		default:
			return nil, fmt.Errorf("%s source must be value, env, or file", field)
		}
	}
	return source, nil
}

func (s *ValueSource) resolve(field string, lookup func(string) (string, bool)) (string, error) {
	value := ""
	if s.Value != nil {
		value = *s.Value
	}
	if s.Env != "" {
		var present bool
		value, present = lookup(s.Env)
		if !present {
			return "", fmt.Errorf("%s environment variable %s: %w", field, s.Env, errUnsetSource)
		}
	}
	if s.File != "" {
		return readValueFile(s.File, field)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s: %w", field, errEmptySource)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s source must contain exactly one value", field)
	}
	return value, nil
}
