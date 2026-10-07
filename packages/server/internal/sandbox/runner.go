package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/server/internal/archive"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/platform/process"
	"github.com/billstark001/latexmk/packages/server/internal/resultarchive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// Run gives each compile attempt a new container, PID namespace and quota-bound
// tmpfs. Only verified bytes travel over stdin/stdout; Docker mounts no host data.
func Run(
	ctx context.Context,
	cfg config.Config,
	req protocol.CompileRequest,
	id string,
	sources []protocol.ProjectFile,
	stamps map[string]int64,
	checkpoint, sourceRoot, destination string,
	exportCheckpoint bool,
) (compile.Output, string, error) {
	if cfg.RunnerImage == "" {
		return compile.Output{}, "", errors.New("isolated runner is not configured")
	}
	if err := ensureDir(destination); err != nil {
		return compile.Output{}, "", err
	}
	warm := checkpoint != ""
	payload := workerRequest{
		Version:          workerProtocolVersion,
		ExportCheckpoint: exportCheckpoint,
		Request:          req,
		RequestID:        id,
		Sources:          sources,
		Warm:             warm,
		MaxFiles:         cfg.MaxFiles,
		MaxSourceBytes:   cfg.MaxExpandedBytes,
		MaxStateBytes:    cfg.MaxCompileCacheBytes,
		MaxArtifactBytes: cfg.MaxArtifactBytes,
		MaxLogBytes:      cfg.MaxLogBytes,
		TimeoutMS:        cfg.CompileTimeout.Milliseconds(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return compile.Output{}, "", err
	}
	if err := os.WriteFile(filepath.Join(destination, "request.json"), raw, 0600); err != nil {
		return compile.Output{}, "", err
	}
	metadata, err := describeFile(destination, "request.json", 4<<20)
	if err != nil {
		return compile.Output{}, "", err
	}
	members := []archiveMember{{name: "request.json", file: metadata}}
	for _, source := range sources {
		members = append(
			members,
			archiveMember{
				name: "sources/" + source.Path,
				file: compile.File{
					Workspace:    sourceRoot,
					RelativePath: source.Path,
					Size:         source.Size,
					SHA256:       source.SHA256,
				},
				modTime: time.Unix(stamps[source.Path], 0),
			},
		)
	}
	if warm {
		file, err := describeFile(filepath.Dir(checkpoint), filepath.Base(checkpoint), cfg.MaxCompileCacheBytes+(1<<20))
		if err != nil {
			return compile.Output{}, "", err
		}
		members = append(members, archiveMember{name: "checkpoint.tar.gz", file: file})
	}
	inputPath := filepath.Join(destination, "input.tar.gz")
	if err := writeArchiveFile(inputPath, members, cfg.RunnerWorkspaceBytes); err != nil {
		return compile.Output{}, "", err
	}
	input, err := os.Open(inputPath)
	if err != nil {
		return compile.Output{}, "", err
	}
	defer func() { _ = input.Close() }()
	container := "latexmk-attempt-" + id
	args := containerArgs(cfg, container)
	root, err := safefs.Open(destination)
	if err != nil {
		return compile.Output{}, "", err
	}
	defer func() { _ = root.Close() }()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = process.Run(
			cleanupCtx,
			process.Spec{Name: "docker", Args: []string{"rm", "--force", container}, MaxOutputBytes: 4096},
		)
	}()
	var executed process.Result
	err = root.WriteExclusive("response.tar.gz", cfg.RunnerWorkspaceBytes*2, func(w io.Writer) error {
		executed = process.Run(
			ctx,
			process.Spec{
				Name:           "docker",
				Args:           args,
				Stdin:          input,
				Stdout:         w,
				MaxStreamBytes: cfg.RunnerWorkspaceBytes * 2,
				MaxOutputBytes: cfg.MaxLogBytes,
			},
		)
		return executed.Err
	})
	if err != nil {
		return compile.Output{}, "", fmt.Errorf("isolated runner failed: %w: %s", err, executed.Stderr)
	}
	response, err := root.OpenRegular("response.tar.gz")
	if err != nil {
		return compile.Output{}, "", err
	}
	defer func() { _ = response.Close() }()
	outer := filepath.Join(destination, "response")
	if err := ensureDir(outer); err != nil {
		return compile.Output{}, "", err
	}
	if _, err := projectarchive.ExtractTarGz(
		response,
		outer,
		projectarchive.Limits{MaxFiles: 2, MaxBytes: cfg.RunnerWorkspaceBytes * 2},
	); err != nil {
		return compile.Output{}, "", workerError(err)
	}
	resultRoot, err := safefs.Open(outer)
	if err != nil {
		return compile.Output{}, "", err
	}
	defer func() { _ = resultRoot.Close() }()
	encoded, err := resultRoot.OpenRegular("result.tar.gz")
	if err != nil {
		return compile.Output{}, "", workerError(err)
	}
	artifactRoot := filepath.Join(destination, "artifacts")
	if err := ensureDir(artifactRoot); err != nil {
		_ = encoded.Close()
		return compile.Output{}, "", err
	}
	result, decodeErr := resultarchive.Decode(
		encoded,
		artifactRoot,
		resultarchive.Limits{MaxFiles: cfg.MaxFiles, MaxArtifacts: cfg.MaxArtifactBytes, MaxLogs: cfg.MaxLogBytes},
	)
	if err := errors.Join(decodeErr, encoded.Close()); err != nil {
		return compile.Output{}, "", workerError(err)
	}
	if result.Result.RequestID != id || result.Result.Entry != req.Entry || result.Result.Engine != req.Engine ||
		result.Result.ProtocolVersion != protocol.Version {
		return compile.Output{}, "", errors.New("runner result does not match its compile attempt")
	}
	selected := make(map[string]bool, len(sources))
	for _, file := range sources {
		selected[file.Path] = true
	}
	for _, name := range result.Result.InputFiles {
		if !selected[name] {
			return compile.Output{}, "", errors.New("runner returned an undeclared dependency")
		}
	}
	for _, name := range result.Result.NeedsFiles {
		if clean, err := safefs.Clean(name); err != nil || clean != name {
			return compile.Output{}, "", errors.New("runner returned an invalid missing-file request")
		}
	}
	if !result.Result.Success {
		return result, "", nil
	}
	cache, err := resultRoot.OpenRegular("checkpoint.tar.gz")
	if !exportCheckpoint && err == nil {
		_ = cache.Close()
		return compile.Output{}, "", errors.New("worker exported an unrequested checkpoint")
	}
	if errors.Is(err, os.ErrNotExist) {
		return result, "", nil
	}
	if err != nil {
		return compile.Output{}, "", err
	}
	defer func() { _ = cache.Close() }()
	validation := filepath.Join(destination, "validated-checkpoint")
	if err := ensureDir(validation); err != nil {
		return compile.Output{}, "", err
	}
	if _, err := projectarchive.ExtractTarGz(
		cache,
		validation,
		projectarchive.Limits{MaxFiles: cfg.MaxFiles, MaxBytes: cfg.MaxCompileCacheBytes},
	); err != nil {
		return compile.Output{}, "", workerError(err)
	}
	return result, filepath.Join(outer, "checkpoint.tar.gz"), nil
}

func containerArgs(cfg config.Config, name string) []string {
	return []string{
		"run",
		"--rm",
		"--pull=never",
		"--name",
		name,
		"--label",
		"latexmk.runner=true",
		"--label",
		"latexmk.runner.namespace=" + cfg.RunnerNamespace,
		"--network=none",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--pids-limit",
		strconv.Itoa(cfg.RunnerPIDs),
		"--memory",
		strconv.FormatInt(cfg.RunnerMemoryBytes, 10),
		"--memory-swap",
		strconv.FormatInt(cfg.RunnerMemoryBytes, 10),
		"--cpus",
		strconv.Itoa(cfg.RunnerCPUs),
		"--user",
		"10001:10001",
		"--tmpfs",
		fmt.Sprintf(
			"/work:rw,nosuid,nodev,noexec,size=%d,nr_inodes=%d,mode=1777",
			cfg.RunnerWorkspaceBytes,
			3*cfg.MaxFiles+100,
		),
		"--tmpfs",
		// Biber PAR unpacks its interpreter and shared libraries here. Execution is
		// already confined by the disposable worker boundary, credentials and quotas.
		"/tmp:rw,nosuid,nodev,exec,size=134217728,nr_inodes=8192,mode=1777",
		"--entrypoint",
		"/usr/local/bin/latexmk-server",
		"-i",
		cfg.RunnerImage,
		"compile-worker",
		strconv.FormatInt(cfg.RunnerWorkspaceBytes, 10),
		strconv.Itoa(3*cfg.MaxFiles + 100),
	}
}

func Validate(ctx context.Context, cfg config.Config) error {
	if cfg.RunnerImage == "" {
		return nil
	}
	result := process.Run(
		ctx,
		process.Spec{
			Name:           "docker",
			Args:           []string{"image", "inspect", "--format", "{{.Id}}", cfg.RunnerImage},
			MaxOutputBytes: 4096,
		},
	)
	if result.Err != nil {
		return fmt.Errorf("isolated runner image must be pulled before startup: %w: %s", result.Err, result.Stderr)
	}
	orphaned := process.Run(
		ctx,
		process.Spec{
			Name:           "docker",
			Args:           []string{"ps", "-aq", "--filter", "label=latexmk.runner.namespace=" + cfg.RunnerNamespace},
			MaxOutputBytes: 1 << 20,
		},
	)
	if orphaned.Err != nil {
		return orphaned.Err
	}
	ids := strings.Fields(string(orphaned.Stdout))
	if len(ids) > 0 {
		args := append([]string{"rm", "--force"}, ids...)
		removed := process.Run(ctx, process.Spec{Name: "docker", Args: args, MaxOutputBytes: 1 << 20})
		if removed.Err != nil {
			return fmt.Errorf("remove orphaned runner attempts: %w", removed.Err)
		}
	}
	return nil
}

func ValidateSourcePaths(files []protocol.ProjectFile) error {
	for _, file := range files {
		for _, reserved := range []string{".latexmk-build", ".latexmk-home"} {
			if file.Path == reserved || strings.HasPrefix(file.Path, reserved+"/") {
				return errors.New("source uses a reserved runner directory")
			}
		}
	}
	return nil
}
