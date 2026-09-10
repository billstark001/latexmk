package config

import (
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	CompileCacheRetention time.Duration
	MaxCompileCacheBytes  int64
	CompileCacheEpoch     string
	Addr                  string
	AuthMode              string
	APIToken              string
	BootstrapToken        string
	DatabaseURL           string
	ImageProfile          string
	Engines               []string
	AllowShellEscape      bool
	EnableLegacyCompile   bool
	CompileTimeout        time.Duration
	ShutdownTimeout       time.Duration
	MaxUploadBytes        int64
	MaxExpandedBytes      int64
	MaxArtifactBytes      int64
	MaxFiles              int
	MaxConcurrentCompiles int
	MaxQueuedJobs         int
	MaxLogBytes           int64
	MaxStateBytes         int64
	MaxUploadSessions     int
	ResultRetention       time.Duration
	SnapshotRetention     time.Duration
	BlobRetention         time.Duration
	StateSweepInterval    time.Duration
	TempDir               string
	StateDir              string
	DatabaseMode          string
	CORSOrigins           []string
}

func Load() (Config, error) {
	allowShellEscape, err := envBool("LATEXMK_ALLOW_SHELL_ESCAPE", false)
	if err != nil {
		return Config{}, err
	}
	enableLegacyCompile, err := envBool("LATEXMK_ENABLE_LEGACY_COMPILE", false)
	if err != nil {
		return Config{}, err
	}
	compileTimeout, err := envDuration("LATEXMK_COMPILE_TIMEOUT", 2*time.Minute)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := envDuration("LATEXMK_SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	maxUploadBytes, err := envBytes("LATEXMK_MAX_UPLOAD_BYTES", 64<<20)
	if err != nil {
		return Config{}, err
	}
	maxExpandedBytes, err := envBytes("LATEXMK_MAX_EXPANDED_BYTES", 256<<20)
	if err != nil {
		return Config{}, err
	}
	maxArtifactBytes, err := envBytes("LATEXMK_MAX_ARTIFACT_BYTES", 128<<20)
	if err != nil {
		return Config{}, err
	}
	maxFiles, err := envInt("LATEXMK_MAX_FILES", 10_000)
	if err != nil {
		return Config{}, err
	}
	maxConcurrent, err := envInt("LATEXMK_MAX_CONCURRENT_COMPILES", max(1, runtime.NumCPU()/2))
	if err != nil {
		return Config{}, err
	}
	maxLogBytes, err := envBytes("LATEXMK_MAX_LOG_BYTES", 8<<20)
	if err != nil {
		return Config{}, err
	}
	maxQueuedJobs, err := envInt("LATEXMK_MAX_QUEUED_JOBS", 100)
	if err != nil {
		return Config{}, err
	}
	maxStateBytes, err := envBytes("LATEXMK_MAX_STATE_BYTES", 2<<30)
	if err != nil {
		return Config{}, err
	}
	maxUploadSessions, err := envInt("LATEXMK_MAX_UPLOAD_SESSIONS", 64)
	if err != nil {
		return Config{}, err
	}
	resultRetention, err := envDuration("LATEXMK_RESULT_RETENTION", 7*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	snapshotRetention, err := envDuration("LATEXMK_SNAPSHOT_RETENTION", 7*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	blobRetention, err := envDuration("LATEXMK_BLOB_RETENTION", 7*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	stateSweepInterval, err := envDuration("LATEXMK_STATE_SWEEP_INTERVAL", time.Hour)
	if err != nil {
		return Config{}, err
	}
	cacheRetention, err := envDuration("LATEXMK_COMPILE_CACHE_RETENTION", 24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	var cacheBytes int64
	if strings.TrimSpace(os.Getenv("LATEXMK_MAX_COMPILE_CACHE_BYTES")) != "0" {
		cacheBytes, err = envBytes("LATEXMK_MAX_COMPILE_CACHE_BYTES", 16<<20)
		if err != nil {
			return Config{}, err
		}
	}
	apiToken, err := loadAPIToken()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		CompileCacheRetention: cacheRetention,
		MaxCompileCacheBytes:  cacheBytes,
		CompileCacheEpoch:     os.Getenv("LATEXMK_COMPILE_CACHE_EPOCH"),
		Addr:                  ":" + env("PORT", "8080"),
		AuthMode:              env("LATEXMK_AUTH_MODE", "token"),
		APIToken:              apiToken,
		BootstrapToken:        os.Getenv("LATEXMK_BOOTSTRAP_TOKEN"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		ImageProfile:          env("LATEXMK_IMAGE_PROFILE", "development"),
		Engines:               splitCSV(env("LATEXMK_ENGINES", "xelatex,lualatex,pdflatex")),
		AllowShellEscape:      allowShellEscape,
		EnableLegacyCompile:   enableLegacyCompile,
		CompileTimeout:        compileTimeout,
		ShutdownTimeout:       shutdownTimeout,
		MaxUploadBytes:        maxUploadBytes,
		MaxExpandedBytes:      maxExpandedBytes,
		MaxArtifactBytes:      maxArtifactBytes,
		MaxFiles:              maxFiles,
		MaxConcurrentCompiles: maxConcurrent,
		MaxQueuedJobs:         maxQueuedJobs,
		MaxLogBytes:           maxLogBytes,
		MaxStateBytes:         maxStateBytes,
		MaxUploadSessions:     maxUploadSessions,
		ResultRetention:       resultRetention,
		SnapshotRetention:     snapshotRetention,
		BlobRetention:         blobRetention,
		StateSweepInterval:    stateSweepInterval,
		TempDir:               os.Getenv("LATEXMK_TEMP_DIR"),
		StateDir:              env("LATEXMK_STATE_DIR", "/tmp/latexmk-state"),
		DatabaseMode:          env("LATEXMK_DATABASE_MODE", "postgres"),
		CORSOrigins:           splitRawCSV(os.Getenv("LATEXMK_CORS_ORIGINS")),
	}
	if v := os.Getenv("LATEXMK_ADDR"); v != "" {
		cfg.Addr = v
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadAPIToken() (string, error) {
	token := os.Getenv("LATEXMK_API_TOKEN")
	path := os.Getenv("LATEXMK_API_TOKEN_FILE")
	if token != "" && path != "" {
		return "", fmt.Errorf("set only one of LATEXMK_API_TOKEN and LATEXMK_API_TOKEN_FILE")
	}
	if path == "" {
		return token, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read LATEXMK_API_TOKEN_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("LATEXMK_API_TOKEN_FILE must be a regular file")
	}
	if info.Size() > 64<<10 {
		return "", fmt.Errorf("LATEXMK_API_TOKEN_FILE is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read LATEXMK_API_TOKEN_FILE: %w", err)
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("LATEXMK_API_TOKEN_FILE is empty")
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("LATEXMK_API_TOKEN_FILE must contain one token")
	}
	return token, nil
}

func (c Config) Validate() error {
	switch c.AuthMode {
	case "none":
	case "token":
		if len(c.APIToken) < 24 {
			return fmt.Errorf("LATEXMK_API_TOKEN must contain at least 24 characters for token auth")
		}
	case "postgres", "database":
		if c.DatabaseURL == "" {
			return fmt.Errorf("DATABASE_URL is required for postgres auth")
		}
		if len(c.BootstrapToken) < 24 {
			return fmt.Errorf("LATEXMK_BOOTSTRAP_TOKEN must contain at least 24 characters for postgres auth")
		}
	default:
		return fmt.Errorf("unsupported auth mode %q", c.AuthMode)
	}
	if len(c.Engines) == 0 {
		return fmt.Errorf("at least one engine must be enabled")
	}
	for _, e := range c.Engines {
		switch e {
		case "xelatex", "lualatex", "pdflatex":
		default:
			return fmt.Errorf("unsupported configured engine %q", e)
		}
	}
	if c.CompileCacheRetention < 0 || c.MaxCompileCacheBytes < 0 {
		return fmt.Errorf("compile cache limits cannot be negative")
	}
	if c.CompileTimeout <= 0 || c.ShutdownTimeout <= 0 || c.MaxUploadBytes <= 0 || c.MaxExpandedBytes <= 0 || c.MaxArtifactBytes <= 0 || c.MaxFiles <= 0 || c.MaxConcurrentCompiles <= 0 || c.MaxQueuedJobs <= 0 || c.MaxLogBytes <= 0 || c.MaxStateBytes <= 0 || c.MaxUploadSessions <= 0 || c.ResultRetention <= 0 || c.SnapshotRetention <= 0 || c.BlobRetention <= 0 || c.StateSweepInterval <= 0 {
		return fmt.Errorf("resource limits must be positive")
	}
	if c.MaxExpandedBytes < c.MaxUploadBytes {
		return fmt.Errorf("LATEXMK_MAX_EXPANDED_BYTES must be at least LATEXMK_MAX_UPLOAD_BYTES")
	}
	if c.StateDir == "" {
		return fmt.Errorf("LATEXMK_STATE_DIR is required")
	}
	if c.DatabaseMode != "postgres" && c.DatabaseMode != "pglite" {
		return fmt.Errorf("LATEXMK_DATABASE_MODE must be postgres or pglite")
	}
	for _, origin := range c.CORSOrigins {
		if !validOrigin(origin) {
			return fmt.Errorf("LATEXMK_CORS_ORIGINS contains invalid exact origin %q", origin)
		}
	}
	return nil
}

func (c Config) EngineAllowed(engine string) bool {
	for _, e := range c.Engines {
		if e == engine {
			return true
		}
	}
	return false
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return parsed, nil
}

func envBytes(name string, fallback int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"gib", 1 << 30}, {"gb", 1000 * 1000 * 1000},
		{"mib", 1 << 20}, {"mb", 1000 * 1000},
		{"kib", 1 << 10}, {"kb", 1000},
	}
	lower := strings.ToLower(v)
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSpace(lower[:len(lower)-len(unit.suffix)]), 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("%s must be a positive byte size", name)
			}
			return int64(n * float64(unit.multiplier)), nil
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive byte size", name)
	}
	return n, nil
}

func splitCSV(value string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(strings.ToLower(item))
		if item != "" && !seen[item] {
			out = append(out, item)
			seen[item] = true
		}
	}
	return out
}

func splitRawCSV(value string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			out = append(out, item)
			seen[item] = true
		}
	}
	return out
}

func validOrigin(value string) bool {
	if value == "" || value == "*" {
		return false
	}
	u, err := url.ParseRequestURI(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Path == ""
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
