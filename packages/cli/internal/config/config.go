// Package config resolves CLI options, environment variables, and project configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

const FileName = ".latexmk.json"
const UserFileName = "config.json"
const EnvFileName = ".env.latexmk"
const TokenFileName = ".latexmk-token"

const maxTokenFileSize = 64 << 10

type Target struct {
	Entry        string   `json:"entry"`
	Engine       string   `json:"engine,omitempty"`
	OutDir       string   `json:"outDir,omitempty"`
	PDF          string   `json:"pdf,omitempty"`
	IncludeFiles []string `json:"includeFiles,omitempty"`
}

type FileConfig struct {
	TokenMode     string            `json:"tokenMode,omitempty"`
	TokenFile     string            `json:"tokenFile,omitempty"`
	EnvFile       *string           `json:"envFile,omitempty"`
	IgnoreFiles   []string          `json:"ignoreFiles"`
	UnmatchedGlob string            `json:"unmatchedGlob,omitempty"`
	OutDir        string            `json:"outDir,omitempty"`
	Targets       map[string]Target `json:"targets,omitempty"`

	Auxiliary          protocol.AuxiliaryOptions `json:"auxiliary,omitempty"`
	Server             string                    `json:"server"`
	Token              string                    `json:"token,omitempty"`
	ProjectRoot        string                    `json:"projectRoot,omitempty"`
	ProjectID          string                    `json:"projectId,omitempty"`
	RootMode           string                    `json:"rootMode,omitempty"`
	UploadMode         string                    `json:"uploadMode,omitempty"`
	ManifestFile       string                    `json:"manifestFile,omitempty"`
	IncludeFiles       []string                  `json:"includeFiles,omitempty"`
	RespectGitIgnore   *bool                     `json:"respectGitignore,omitempty"`
	Engine             string                    `json:"engine,omitempty"`
	Timeout            string                    `json:"timeout,omitempty"`
	Exclude            []string                  `json:"exclude,omitempty"`
	InsecureSkipVerify bool                      `json:"insecureSkipVerify,omitempty"`
}

type Resolved struct {
	TokenMode     string
	TokenSource   string
	EnvPath       string
	IgnoreFiles   []string
	UnmatchedGlob string
	OutDir        string
	Targets       map[string]Target
	DenyFiles     []string
	auth          credentials

	Auxiliary          protocol.AuxiliaryOptions
	Server             string
	Token              string
	ProjectRoot        string
	ProjectID          string
	RootMode           string
	UploadMode         string
	ManifestFile       string
	IncludeFiles       []string
	RespectGitIgnore   bool
	Engine             string
	Timeout            time.Duration
	Exclude            []string
	InsecureSkipVerify bool
	ConfigPath         string
	UserConfigPath     string
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
		EnvFileName, TokenFileName, ".latexmk.env", ".latexmk-manifest", ".latexmk-cache/",
		".latexmkignore",
		".latexmk-files",
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

func load(start string, envOverride *string) (Resolved, error) {
	respectGitIgnore := true
	cfg := FileConfig{
		TokenMode: "auto", UnmatchedGlob: "error",
		Server:           "http://127.0.0.1:8080",
		RootMode:         "entry",
		UploadMode:       "auto",
		RespectGitIgnore: &respectGitIgnore,
		Engine:           "xelatex",
		Timeout:          "3m",
		Exclude:          DefaultExcludes(),
	}
	userDir, err := userConfigDirectory()
	if err != nil {
		return Resolved{}, err
	}
	userPath, err := findUserConfig(userDir)
	if err != nil {
		return Resolved{}, err
	}
	if userPath != "" {
		if err := mergeFile(userPath, &cfg); err != nil {
			return Resolved{}, err
		}
	}
	userToken, userTokenFile := cfg.Token, cfg.TokenFile

	path, err := findConfig(start)
	if err != nil {
		return Resolved{}, err
	}
	if path != "" {
		if err := mergeFile(path, &cfg); err != nil {
			return Resolved{}, err
		}
	}
	projectToken, projectTokenFile := cfg.Token, cfg.TokenFile
	if userToken != "" || userTokenFile != "" {
		cfg.Token, cfg.TokenFile = userToken, userTokenFile
	}
	envPath, envValues, err := loadEnvironment(start, path, cfg.EnvFile, envOverride)
	if err != nil {
		return Resolved{}, err
	}
	get := func(name string) string {
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return envValues[name]
	}

	cfg.Exclude = mergePatterns(cfg.Exclude, DefaultDeny())

	if v := get("LATEXMK_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := get("LATEXMK_ENGINE"); v != "" {
		cfg.Engine = v
	}
	if v := get("LATEXMK_PROJECT_ID"); v != "" {
		cfg.ProjectID = v
	}
	if v := get("LATEXMK_ROOT_MODE"); v != "" {
		cfg.RootMode = v
	}
	if v := get("LATEXMK_UPLOAD_MODE"); v != "" {
		cfg.UploadMode = v
	}
	if v := get("LATEXMK_MANIFEST_FILE"); v != "" {
		cfg.ManifestFile = v
	}
	if v := get("LATEXMK_RESPECT_GITIGNORE"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return Resolved{}, fmt.Errorf("invalid LATEXMK_RESPECT_GITIGNORE %q: %w", v, err)
		}
		cfg.RespectGitIgnore = &parsed
	}
	if v := get("LATEXMK_SERVER_CACHE"); v != "" {
		cfg.Auxiliary.Server = v
	}
	if v := get("LATEXMK_SERVER_CACHE_TTL"); v != "" {
		cfg.Auxiliary.ServerTTL = v
	}
	if cfg.Auxiliary.Server != "" && cfg.Auxiliary.Server != "none" && cfg.Auxiliary.Server != "reuse" &&
		cfg.Auxiliary.Server != "retain" {
		return Resolved{}, errors.New("auxiliary.server must be none, retain, or reuse")
	}
	if cfg.RootMode != "entry" && cfg.RootMode != "git" {
		return Resolved{}, fmt.Errorf("invalid rootMode %q; expected entry or git", cfg.RootMode)
	}
	if cfg.UploadMode == "ignore" {
		cfg.UploadMode = "all"
	}
	if cfg.UploadMode != "auto" && cfg.UploadMode != "manifest" && cfg.UploadMode != "all" {
		return Resolved{}, fmt.Errorf("invalid uploadMode %q; expected auto, manifest, or all", cfg.UploadMode)
	}

	if v := get("LATEXMK_TOKEN_MODE"); v != "" {
		cfg.TokenMode = v
	}
	if v := get("LATEXMK_LOCAL_CACHE"); v != "" {
		cfg.Auxiliary.Local = v
	}
	if v := get("LATEXMK_UNMATCHED_GLOB"); v != "" {
		cfg.UnmatchedGlob = v
	}
	if cfg.UnmatchedGlob != "error" && cfg.UnmatchedGlob != "warn" && cfg.UnmatchedGlob != "ignore" {
		return Resolved{}, errors.New("unmatchedGlob must be error, warn, or ignore")
	}
	if cfg.Auxiliary.Local != "" && cfg.Auxiliary.Local != "none" && cfg.Auxiliary.Local != "cache" &&
		cfg.Auxiliary.Local != "output" {
		return Resolved{}, errors.New("auxiliary.local must be none, cache, or output")
	}
	if cfg.Auxiliary.ServerTTL != "" {
		ttl, err := time.ParseDuration(cfg.Auxiliary.ServerTTL)
		if err != nil || ttl <= 0 {
			return Resolved{}, errors.New("auxiliary.serverTTL must be a positive duration")
		}
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
		TokenMode:     cfg.TokenMode,
		EnvPath:       envPath,
		IgnoreFiles:   cfg.IgnoreFiles,
		UnmatchedGlob: cfg.UnmatchedGlob,
		OutDir:        cfg.OutDir,
		Targets:       cfg.Targets,
		DenyFiles: []string{
			userPath,
			filepath.Join(userDir, "token"),
			envPath,
			cfg.TokenFile,
			userTokenFile,
			projectTokenFile,
			environmentPath(get("LATEXMK_TOKEN_FILE"), envPath, "LATEXMK_TOKEN_FILE"),
		},
		auth: credentials{
			userDefaultFile: filepath.Join(userDir, "token"),
			userToken:       userToken,
			userFile:        userTokenFile,
			projectToken:    projectToken,
			projectFile:     projectTokenFile,
			envToken: get(
				"LATEXMK_TOKEN",
			),
			envFile: environmentPath(get("LATEXMK_TOKEN_FILE"), envPath, "LATEXMK_TOKEN_FILE"),
			start:   start,
		},
		Auxiliary:          cfg.Auxiliary,
		Server:             cfg.Server,
		Token:              cfg.Token,
		ProjectRoot:        resolvedRoot,
		ProjectID:          cfg.ProjectID,
		RootMode:           cfg.RootMode,
		UploadMode:         cfg.UploadMode,
		ManifestFile:       cfg.ManifestFile,
		IncludeFiles:       append([]string(nil), cfg.IncludeFiles...),
		RespectGitIgnore:   respectGitIgnore,
		Engine:             cfg.Engine,
		Timeout:            timeout,
		Exclude:            cfg.Exclude,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		ConfigPath:         path,
		UserConfigPath:     userPath,
	}, nil
}

func mergeFile(path string, cfg *FileConfig) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	// Existing configurations downloaded auxiliaries and retained them in job
	// results. A local policy opts into the new independent retention semantics.
	if cfg.Auxiliary.Local == "" {
		cfg.Auxiliary.Local = "output"
		if cfg.Auxiliary.Server == "" {
			cfg.Auxiliary.Server = "retain"
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if _, ok := fields["outDir"]; ok && cfg.OutDir != "" && !filepath.IsAbs(cfg.OutDir) {
		cfg.OutDir = filepath.Join(filepath.Dir(path), cfg.OutDir)
	}
	if _, ok := fields["projectRoot"]; ok && cfg.ProjectRoot != "" && !filepath.IsAbs(cfg.ProjectRoot) {
		cfg.ProjectRoot = filepath.Join(filepath.Dir(path), cfg.ProjectRoot)
	}
	if raw, ok := fields["targets"]; ok {
		var declared map[string]Target
		if err := json.Unmarshal(raw, &declared); err != nil {
			return err
		}
		for name := range declared {
			target := cfg.Targets[name]
			if target.Entry != "" && !filepath.IsAbs(target.Entry) {
				target.Entry = filepath.Join(filepath.Dir(path), target.Entry)
			}
			if target.OutDir != "" && !filepath.IsAbs(target.OutDir) {
				target.OutDir = filepath.Join(filepath.Dir(path), target.OutDir)
			}
			cfg.Targets[name] = target
		}
	}
	if _, ok := fields["tokenFile"]; ok && cfg.TokenFile != "" && !filepath.IsAbs(cfg.TokenFile) {
		cfg.TokenFile = filepath.Join(filepath.Dir(path), cfg.TokenFile)
	}
	if _, ok := fields["envFile"]; ok && cfg.EnvFile != nil && *cfg.EnvFile != "" && !filepath.IsAbs(*cfg.EnvFile) {
		value := filepath.Join(filepath.Dir(path), *cfg.EnvFile)
		cfg.EnvFile = &value
	}
	return nil
}

func userConfigDirectory() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("find user config directory: %w", err)
		}
	}
	return filepath.Join(base, "latexmk"), nil
}

func findUserConfig(base string) (string, error) {
	path := filepath.Join(base, UserFileName)
	st, err := os.Stat(path)
	if err == nil {
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("user config %s is not a regular file", path)
		}
		return path, nil
	}
	if os.IsNotExist(err) {
		return "", nil
	}
	return "", fmt.Errorf("stat user config %s: %w", path, err)
}

// ReadTokenFile reads one bearer token from a regular file. Leading and
// trailing whitespace is ignored to support Docker and Kubernetes secrets.
func ReadTokenFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	if st.Size() > maxTokenFileSize {
		return "", fmt.Errorf("token file %s exceeds %d bytes", path, maxTokenFileSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFileSize+1))
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	if len(b) > maxTokenFileSize {
		return "", fmt.Errorf("token file %s exceeds %d bytes", path, maxTokenFileSize)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("token file %s must contain exactly one token", path)
	}
	return token, nil
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
	if cfg.UploadMode == "" {
		cfg.UploadMode = "auto"
	}
	if cfg.RespectGitIgnore == nil {
		value := true
		cfg.RespectGitIgnore = &value
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "3m"
	}
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
