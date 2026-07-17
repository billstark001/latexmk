package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const FileName = ".latexmk.json"

type FileConfig struct {
	Server             string   `json:"server"`
	Token              string   `json:"token,omitempty"`
	ProjectRoot        string   `json:"projectRoot,omitempty"`
	RootMode           string   `json:"rootMode,omitempty"`
	RespectGitIgnore   *bool    `json:"respectGitignore,omitempty"`
	Engine             string   `json:"engine,omitempty"`
	Timeout            string   `json:"timeout,omitempty"`
	Exclude            []string `json:"exclude,omitempty"`
	InsecureSkipVerify bool     `json:"insecureSkipVerify,omitempty"`
}

type Resolved struct {
	Server             string
	Token              string
	ProjectRoot        string
	RootMode           string
	RespectGitIgnore   bool
	Engine             string
	Timeout            time.Duration
	Exclude            []string
	InsecureSkipVerify bool
	ConfigPath         string
}

// DefaultExcludes returns files that should not be uploaded without an
// explicit configuration override.
func DefaultExcludes() []string {
	return []string{
		".git",
		".gitignore",
		"node_modules",
		".latexmk-cache",
		"*.aux",
		"*.fdb_latexmk",
		"*.fls",
		"*.log",
		"*.synctex.gz",
		"*.xdv",
	}
}

// DefaultDeny returns local configuration and credential patterns that remain
// excluded even when a project replaces the ordinary exclude list.
func DefaultDeny() []string {
	return []string{
		FileName,
		".latexmkignore",
		".env",
		".env.*",
		"*.key",
		"*.pem",
		"*.p12",
		"*.pfx",
		"id_rsa",
		"id_ed25519",
	}
}

func Load(start string) (Resolved, error) {
	respectGitIgnore := true
	cfg := FileConfig{
		Server:           "http://127.0.0.1:8080",
		RootMode:         "entry",
		RespectGitIgnore: &respectGitIgnore,
		Engine:           "xelatex",
		Timeout:          "3m",
		Exclude:          DefaultExcludes(),
	}
	path, err := findConfig(start)
	if err != nil {
		return Resolved{}, err
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Resolved{}, fmt.Errorf("read %s: %w", path, err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return Resolved{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg.Exclude = mergePatterns(cfg.Exclude, DefaultDeny())

	if v := os.Getenv("LATEXMK_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv("LATEXMK_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("LATEXMK_ENGINE"); v != "" {
		cfg.Engine = v
	}
	if v := os.Getenv("LATEXMK_ROOT_MODE"); v != "" {
		cfg.RootMode = v
	}
	if v := os.Getenv("LATEXMK_RESPECT_GITIGNORE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return Resolved{}, fmt.Errorf("invalid LATEXMK_RESPECT_GITIGNORE %q: %w", v, err)
		}
		cfg.RespectGitIgnore = &parsed
	}
	if cfg.RootMode != "entry" && cfg.RootMode != "git" {
		return Resolved{}, fmt.Errorf("invalid rootMode %q; expected entry or git", cfg.RootMode)
	}

	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		return Resolved{}, fmt.Errorf("invalid timeout %q: %w", cfg.Timeout, err)
	}
	if timeout <= 0 {
		return Resolved{}, errors.New("timeout must be positive")
	}

	root := cfg.ProjectRoot
	if root != "" && !filepath.IsAbs(root) {
		base := start
		if path != "" {
			base = filepath.Dir(path)
		}
		root = filepath.Join(base, root)
	}
	if root != "" {
		root, err = filepath.Abs(root)
		if err != nil {
			return Resolved{}, err
		}
	}

	resolvedRoot := ""
	if root != "" {
		resolvedRoot = filepath.Clean(root)
	}
	respectGitIgnore = cfg.RespectGitIgnore == nil || *cfg.RespectGitIgnore
	return Resolved{
		Server:             cfg.Server,
		Token:              cfg.Token,
		ProjectRoot:        resolvedRoot,
		RootMode:           cfg.RootMode,
		RespectGitIgnore:   respectGitIgnore,
		Engine:             cfg.Engine,
		Timeout:            timeout,
		Exclude:            cfg.Exclude,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		ConfigPath:         path,
	}, nil
}

func mergePatterns(base, required []string) []string {
	result := append([]string{}, base...)
	seen := make(map[string]struct{}, len(result))
	for _, pattern := range result {
		seen[pattern] = struct{}{}
	}
	for _, pattern := range required {
		if _, ok := seen[pattern]; ok {
			continue
		}
		result = append(result, pattern)
		seen[pattern] = struct{}{}
	}
	return result
}

func Write(path string, cfg FileConfig) error {
	if cfg.Server == "" {
		cfg.Server = "http://127.0.0.1:8080"
	}
	if cfg.Engine == "" {
		cfg.Engine = "xelatex"
	}
	if cfg.RootMode == "" {
		cfg.RootMode = "entry"
	}
	if cfg.RespectGitIgnore == nil {
		value := true
		cfg.RespectGitIgnore = &value
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "3m"
	}
	if len(cfg.Exclude) == 0 {
		cfg.Exclude = DefaultExcludes()
	}
	cfg.Exclude = mergePatterns(cfg.Exclude, DefaultDeny())
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

func findConfig(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// FindGitRoot returns the nearest Git work tree root above start.
func FindGitRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no Git root found from %s", start)
		}
		dir = parent
	}
}
