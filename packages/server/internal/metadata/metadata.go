// Package metadata reports service capabilities and installed toolchain versions.
package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/platform/process"
	"github.com/billstark001/latexmk/packages/shared/engine"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type BuildInfo struct {
	Version   string
	Commit    string
	BuildDate string
}

const (
	maxConcurrentProbes  = 4
	versionProbeTimeout  = 3 * time.Second
	maxVersionProbeBytes = 4096
	maxVersionLineBytes  = 300
)

type toolProbe struct {
	command engine.Command
	names   []string
}

// The callback must be safe for concurrent use. Exact executable/argument pairs
// share one probe, including aliases registered under different engine names.
func collectToolchain(probe func(string, ...string) string) map[string]string {
	var probes []toolProbe
	indices := make(map[string]int)
	add := func(name string, command engine.Command) {
		raw, _ := json.Marshal(command)
		key := string(raw)
		if index, exists := indices[key]; exists {
			probes[index].names = append(probes[index].names, name)
			return
		}
		indices[key] = len(probes)
		probes = append(probes, toolProbe{command: command, names: []string{name}})
	}
	for _, tool := range []engine.Command{{Name: "latexmk", Args: []string{"-v"}}, {Name: "biber", Args: []string{"--version"}}, {Name: "kpsewhich", Args: []string{"--version"}}} {
		add(tool.Name, tool)
	}
	for _, name := range engine.Default.Names() {
		driver, _ := engine.Default.Lookup(name)
		add(name, driver.VersionProbe())
	}
	results := make([]string, len(probes))
	slots := make(chan struct{}, maxConcurrentProbes)
	var group sync.WaitGroup
	for index, task := range probes {
		slots <- struct{}{}
		group.Go(func() { defer func() { <-slots }(); results[index] = probe(task.command.Name, task.command.Args...) })
	}
	group.Wait()
	toolchain := make(map[string]string, len(probes))
	for index, task := range probes {
		if results[index] != "" {
			for _, name := range task.names {
				toolchain[name] = results[index]
			}
		}
	}
	return toolchain
}

// Collect probes registered tool versions and describes the configured service.
// Individual probes have a short deadline; unavailable optional tools are omitted.
func Collect(cfg config.Config, build BuildInfo) protocol.Metadata {
	toolchain := collectToolchain(firstLine)
	database := "disabled"
	if cfg.DatabaseURL != "" {
		if cfg.DatabaseMode == "pglite" {
			database = "pglite"
		} else {
			database = "postgresql"
		}
	}
	return protocol.Metadata{
		ProtocolVersion: protocol.Version,
		Service:         "latexmk",
		Version:         build.Version,
		Commit:          build.Commit,
		BuildDate:       build.BuildDate,
		ImageProfile:    cfg.ImageProfile,
		AuthMode:        cfg.AuthMode,
		Database:        database,
		Capabilities: protocol.Capabilities{
			RealtimeSessions:        cfg.MaxRealtimeSessions > 0,
			IsolatedWorkspaces:      cfg.RunnerImage != "",
			MaxRealtimeSessions:     cfg.MaxRealtimeSessions,
			SessionTTLMS:            cfg.RealtimeSessionTTL.Milliseconds(),
			CompileCache:            cfg.CompileCacheRetention > 0 && cfg.MaxCompileCacheBytes > 0,
			AuxiliaryRetention:      true,
			CompileCacheRetentionMS: cfg.CompileCacheRetention.Milliseconds(),
			Engines:                 append([]string(nil), cfg.Engines...),
			MaxUploadBytes:          cfg.MaxUploadBytes,
			MaxExpandedBytes:        cfg.MaxExpandedBytes,
			MaxFiles:                cfg.MaxFiles,
			MaxArtifactBytes:        cfg.MaxArtifactBytes,
			CompileTimeoutMS:        cfg.CompileTimeout.Milliseconds(),
			MaxConcurrent:           cfg.MaxConcurrentCompiles,
			ShellEscapeAllowed:      cfg.AllowShellEscape,
			ProjectRCFilesRead:      false,
			PersistentWorkspace:     true,
			IncrementalUpload:       true,
			QueuedJobs:              true,
			DependencyInputs:        true,
			NeedsFiles:              true,
			MaxQueuedJobs:           cfg.MaxQueuedJobs,
			MaxStateBytes:           cfg.MaxStateBytes,
			MaxUploadSessions:       cfg.MaxUploadSessions,
			ResultRetentionMS:       cfg.ResultRetention.Milliseconds(),
			SnapshotRetentionMS:     cfg.SnapshotRetention.Milliseconds(),
			BlobRetentionMS:         cfg.BlobRetention.Milliseconds(),
			RemoteCleanup:           true,
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

// ValidateToolchain requires latexmk and each enabled engine to have a
// successful version probe before the service accepts compilation requests.
func ValidateToolchain(meta protocol.Metadata, cfg config.Config) error {
	required := append([]string{"latexmk"}, cfg.Engines...)
	for _, tool := range required {
		if meta.Toolchain[tool] == "" {
			return fmt.Errorf("required tool %q is not available", tool)
		}
	}
	return nil
}

func firstLine(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	result := process.Run(
		ctx,
		process.Spec{Name: name, Args: args, MaxOutputBytes: maxVersionProbeBytes, CombinedOutput: true},
	)
	out, err := result.Stdout, result.Err
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > maxVersionLineBytes {
		line = line[:maxVersionLineBytes]
	}
	return line
}
