package metadata

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/config"
)

type BuildInfo struct {
	Version   string
	Commit    string
	BuildDate string
}

func Collect(cfg config.Config, build BuildInfo) api.Metadata {
	toolchain := map[string]string{}
	for _, tool := range []struct {
		name string
		args []string
	}{
		{"latexmk", []string{"-v"}},
		{"xelatex", []string{"--version"}},
		{"lualatex", []string{"--version"}},
		{"pdflatex", []string{"--version"}},
		{"biber", []string{"--version"}},
		{"kpsewhich", []string{"--version"}},
	} {
		if line := firstLine(tool.name, tool.args...); line != "" {
			toolchain[tool.name] = line
		}
	}
	database := "disabled"
	if cfg.DatabaseURL != "" {
		if cfg.DatabaseMode == "pglite" {
			database = "pglite"
		} else {
			database = "postgresql"
		}
	}
	return api.Metadata{
		ProtocolVersion: api.ProtocolVersion,
		Service:         "latexmk",
		Version:         build.Version,
		Commit:          build.Commit,
		BuildDate:       build.BuildDate,
		ImageProfile:    cfg.ImageProfile,
		AuthMode:        cfg.AuthMode,
		Database:        database,
		Capabilities: api.Capabilities{
			Engines:             append([]string(nil), cfg.Engines...),
			MaxUploadBytes:      cfg.MaxUploadBytes,
			MaxExpandedBytes:    cfg.MaxExpandedBytes,
			MaxFiles:            cfg.MaxFiles,
			MaxArtifactBytes:    cfg.MaxArtifactBytes,
			CompileTimeoutMS:    cfg.CompileTimeout.Milliseconds(),
			MaxConcurrent:       cfg.MaxConcurrentCompiles,
			ShellEscapeAllowed:  cfg.AllowShellEscape,
			ProjectRCFilesRead:  false,
			PersistentWorkspace: true,
			IncrementalUpload:   true,
			QueuedJobs:          true,
			DependencyInputs:    true,
			NeedsFiles:          true,
			MaxQueuedJobs:       cfg.MaxQueuedJobs,
			MaxStateBytes:       cfg.MaxStateBytes,
			MaxUploadSessions:   cfg.MaxUploadSessions,
			ResultRetentionMS:   cfg.ResultRetention.Milliseconds(),
			SnapshotRetentionMS: cfg.SnapshotRetention.Milliseconds(),
			BlobRetentionMS:     cfg.BlobRetention.Milliseconds(),
		},
		Toolchain: toolchain,
		Runtime: map[string]string{
			"go":   runtime.Version(),
			"os":   runtime.GOOS,
			"arch": runtime.GOARCH,
		},
		Timestamp: time.Now().UTC(),
	}
}

func ValidateToolchain(meta api.Metadata, cfg config.Config) error {
	required := append([]string{"latexmk"}, cfg.Engines...)
	for _, tool := range required {
		if meta.Toolchain[tool] == "" {
			return fmt.Errorf("required tool %q is not available", tool)
		}
	}
	return nil
}

func firstLine(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 300 {
		line = line[:300]
	}
	return line
}
