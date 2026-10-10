// Package config loads and validates the compiler service configuration.
package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/shared/engine"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type Config struct {
	MaxRealtimeRevisionRate     int
	RunnerNamespace             string
	MaxRealtimeSessions         int
	MaxRealtimeSessionsPerOwner int
	RealtimeSessionTTL          time.Duration
	RunnerImage                 string
	RunnerMemoryBytes           int64
	RunnerWorkspaceBytes        int64
	RunnerPIDs                  int
	RunnerCPUs                  int

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

// CompileCacheMetadataBytes reserves room for the source manifest alongside
// base64-encoded auxiliary files in a serialized compile-cache record.
const CompileCacheMetadataBytes = 16 << 20

// RunnerEntryMultiplier budgets source, build and checkpoint entries per file.
const RunnerEntryMultiplier = 3

// RunnerExtraEntries reserves worker metadata, home and transport directories.
const RunnerExtraEntries = 100

// CheckpointArchiveOverheadBytes reserves compressed transport space beyond the
// checkpoint's raw-file budget. Exceeding it produces a cold next attempt.
const CheckpointArchiveOverheadBytes = 1 << 20

// Load reads startup configuration from the environment, resolves the optional
// token file, and validates all resource and authentication settings.
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
	maxSessions := 0
	if strings.TrimSpace(os.Getenv("LATEXMK_MAX_REALTIME_SESSIONS")) != "0" {
		maxSessions, err = envInt("LATEXMK_MAX_REALTIME_SESSIONS", 16)
		if err != nil {
			return Config{}, err
		}
	}
	revisionRate, err := envInt("LATEXMK_MAX_REALTIME_REVISION_RATE", 5)
	if err != nil {
		return Config{}, err
	}
	ownerSessions, err := envInt("LATEXMK_MAX_REALTIME_SESSIONS_PER_OWNER", 4)
	if err != nil {
		return Config{}, err
	}
	sessionTTL, err := envDuration("LATEXMK_REALTIME_SESSION_TTL", 10*time.Minute)
	if err != nil {
		return Config{}, err
	}
	runnerMemory, err := envBytes("LATEXMK_RUNNER_MEMORY_BYTES", 1<<30)
	if err != nil {
		return Config{}, err
	}
	runnerWorkspace, err := envBytes("LATEXMK_RUNNER_WORKSPACE_BYTES", 512<<20)
	if err != nil {
		return Config{}, err
	}
	runnerPIDs, err := envInt("LATEXMK_RUNNER_PIDS", 128)
	if err != nil {
		return Config{}, err
	}
	runnerCPUs, err := envInt("LATEXMK_RUNNER_CPUS", 2)
	if err != nil {
		return Config{}, err
	}
	apiToken, err := loadAPIToken()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		MaxRealtimeRevisionRate:     revisionRate,
		MaxRealtimeSessions:         maxSessions,
		MaxRealtimeSessionsPerOwner: ownerSessions,
		RealtimeSessionTTL:          sessionTTL,
		RunnerImage:                 strings.TrimSpace(os.Getenv("LATEXMK_RUNNER_IMAGE")),
		RunnerNamespace:             strings.TrimSpace(os.Getenv("LATEXMK_RUNNER_NAMESPACE")),
		RunnerMemoryBytes:           runnerMemory,
		RunnerWorkspaceBytes:        runnerWorkspace,
		RunnerPIDs:                  runnerPIDs,
		RunnerCPUs:                  runnerCPUs,
		CompileCacheRetention:       cacheRetention,
		MaxCompileCacheBytes:        cacheBytes,
		CompileCacheEpoch:           os.Getenv("LATEXMK_COMPILE_CACHE_EPOCH"),
		Addr:                        ":" + env("PORT", "8080"),
		AuthMode:                    env("LATEXMK_AUTH_MODE", "token"),
		APIToken:                    apiToken,
		BootstrapToken:              os.Getenv("LATEXMK_BOOTSTRAP_TOKEN"),
		DatabaseURL:                 os.Getenv("DATABASE_URL"),
		ImageProfile:                env("LATEXMK_IMAGE_PROFILE", "development"),
		Engines:                     splitCSV(env("LATEXMK_ENGINES", "xelatex,lualatex,pdflatex")),
		AllowShellEscape:            allowShellEscape,
		EnableLegacyCompile:         enableLegacyCompile,
		CompileTimeout:              compileTimeout,
		ShutdownTimeout:             shutdownTimeout,
		MaxUploadBytes:              maxUploadBytes,
		MaxExpandedBytes:            maxExpandedBytes,
		MaxArtifactBytes:            maxArtifactBytes,
		MaxFiles:                    maxFiles,
		MaxConcurrentCompiles:       maxConcurrent,
		MaxQueuedJobs:               maxQueuedJobs,
		MaxLogBytes:                 maxLogBytes,
		MaxStateBytes:               maxStateBytes,
		MaxUploadSessions:           maxUploadSessions,
		ResultRetention:             resultRetention,
		SnapshotRetention:           snapshotRetention,
		BlobRetention:               blobRetention,
		StateSweepInterval:          stateSweepInterval,
		TempDir:                     os.Getenv("LATEXMK_TEMP_DIR"),
		StateDir:                    env("LATEXMK_STATE_DIR", "/tmp/latexmk-state"),
		DatabaseMode:                env("LATEXMK_DATABASE_MODE", "postgres"),
		CORSOrigins:                 splitCSV(os.Getenv("LATEXMK_CORS_ORIGINS")),
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
	f, err := safefs.OpenRegularFile(path)
	if err != nil {
		return "", fmt.Errorf("read LATEXMK_API_TOKEN_FILE: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := safefs.ReadLimited(f, 64<<10)
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

// Validate checks a complete configuration, including isolated-runner settings.
// Compile-cache bytes and realtime session count may be zero to disable them.
func (c Config) Validate() error {
	if c.MaxRealtimeSessions < 0 || c.RealtimeSessionTTL <= 0 || c.RunnerPIDs <= 0 || c.RunnerCPUs <= 0 ||
		c.RunnerMemoryBytes <= 0 ||
		c.RunnerWorkspaceBytes <= 0 ||
		c.MaxRealtimeRevisionRate <= 0 ||
		c.MaxRealtimeSessionsPerOwner <= 0 {
		return fmt.Errorf("realtime session and runner limits must be positive")
	}
	if c.RunnerImage != "" &&
		(len(c.RunnerNamespace) < 8 || len(c.RunnerNamespace) > 64 || strings.Trim(c.RunnerNamespace, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "") {
		return fmt.Errorf("LATEXMK_RUNNER_NAMESPACE must be a unique 8-64 character lowercase identifier")
	}
	if c.RunnerImage != "" && !validRunnerImage(c.RunnerImage) {
		return fmt.Errorf("LATEXMK_RUNNER_IMAGE must be an immutable sha256 image reference")
	}

	if c.RunnerImage != "" && c.CompileTimeout < time.Millisecond {
		return fmt.Errorf("isolated runner compile timeout must be at least 1ms")
	}
	if c.RunnerImage != "" && (c.EnableLegacyCompile || c.AllowShellEscape) {
		return fmt.Errorf("isolated runner requires legacy synchronous compilation and shell escape to be disabled")
	}
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
		if _, err := engine.Default.Lookup(e); err != nil {
			return fmt.Errorf("configured engine: %w", err)
		}
	}
	if c.CompileCacheRetention < 0 || c.MaxCompileCacheBytes < 0 {
		return fmt.Errorf("compile cache limits cannot be negative")
	}
	// Derived envelopes double these budgets, and bounded readers need one
	// additional byte to detect excess content without overflowing int64.
	if c.MaxCompileCacheBytes > (math.MaxInt64-CompileCacheMetadataBytes-1)/2 {
		return fmt.Errorf("LATEXMK_MAX_COMPILE_CACHE_BYTES is too large for its serialized envelope")
	}
	if c.RunnerWorkspaceBytes > (math.MaxInt64-1)/2 {
		return fmt.Errorf("LATEXMK_RUNNER_WORKSPACE_BYTES is too large for its transport envelope")
	}
	if c.CompileTimeout <= 0 || c.ShutdownTimeout <= 0 || c.MaxUploadBytes <= 0 || c.MaxExpandedBytes <= 0 ||
		c.MaxArtifactBytes <= 0 ||
		c.MaxFiles <= 0 ||
		c.MaxConcurrentCompiles <= 0 ||
		c.MaxQueuedJobs <= 0 ||
		c.MaxLogBytes <= 0 ||
		c.MaxStateBytes <= 0 ||
		c.MaxUploadSessions <= 0 ||
		c.ResultRetention <= 0 ||
		c.SnapshotRetention <= 0 ||
		c.BlobRetention <= 0 ||
		c.StateSweepInterval <= 0 {
		return fmt.Errorf("resource limits must be positive")
	}
	if c.MaxQueuedJobs > math.MaxInt/2 {
		return fmt.Errorf("LATEXMK_MAX_QUEUED_JOBS is too large for its dispatch queue")
	}
	if c.MaxFiles > (math.MaxInt-RunnerExtraEntries)/RunnerEntryMultiplier {
		return fmt.Errorf("LATEXMK_MAX_FILES is too large for its runner entry budget")
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

// EngineAllowed reports whether the exact registered name is enabled.
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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" {
		return false
	}
	return u.Path == ""
}

func validRunnerImage(value string) bool {
	parts := strings.Split(value, "@sha256:")
	if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 64 {
		return false
	}
	for _, c := range parts[0] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/:-", c)) {
			return false
		}
	}
	for _, c := range parts[1] {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
