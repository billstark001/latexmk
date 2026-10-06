package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	projectarchive "github.com/billstark001/latexmk/packages/server/internal/archive"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/platform/safefs"
	"github.com/billstark001/latexmk/packages/server/internal/resultarchive"
)

type workerRequest struct {
	Version          int                `json:"version"`
	Request          api.CompileRequest `json:"request"`
	RequestID        string             `json:"requestId"`
	Sources          []api.ProjectFile  `json:"sources"`
	Warm             bool               `json:"warm"`
	MaxFiles         int                `json:"maxFiles"`
	MaxSourceBytes   int64              `json:"maxSourceBytes"`
	MaxStateBytes    int64              `json:"maxStateBytes"`
	MaxArtifactBytes int64              `json:"maxArtifactBytes"`
	MaxLogBytes      int64              `json:"maxLogBytes"`
	TimeoutMS        int64              `json:"timeoutMs"`
}

// Worker runs only inside a dedicated disposable container. Its fixed logical
// path makes .fdb_latexmk and recorder paths valid across successful checkpoints.
func Worker(input io.Reader, output io.Writer, maxBytes int64, maxFiles int) error {
	return runWorker(context.Background(), input, output, "/work", maxBytes, maxFiles)
}

func runWorker(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	root string,
	maxBytes int64,
	maxFiles int,
) error {
	inbox := filepath.Join(root, "inbox")
	if err := ensureDir(inbox); err != nil {
		return err
	}
	if _, err := projectarchive.ExtractTarGz(
		input,
		inbox,
		projectarchive.Limits{MaxBytes: maxBytes, MaxFiles: maxFiles},
	); err != nil {
		return err
	}
	scoped, err := safefs.Open(inbox)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	raw, err := scoped.ReadLimited("request.json", 4<<20)
	if err != nil {
		return err
	}
	var req workerRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	if req.Version != 1 || req.Request.ShellEscape || req.MaxFiles <= 0 || req.MaxSourceBytes <= 0 ||
		req.MaxArtifactBytes <= 0 ||
		req.MaxStateBytes < 0 || (req.Warm && req.MaxStateBytes == 0) ||
		req.MaxLogBytes <= 0 ||
		req.TimeoutMS <= 0 {
		return errors.New("invalid worker protocol or limits")
	}
	project := filepath.Join(root, "project")
	if err := os.Rename(filepath.Join(inbox, "sources"), project); err != nil {
		return err
	}
	sourceFS, err := safefs.Open(project)
	if err != nil {
		return err
	}
	defer func() { _ = sourceFS.Close() }()
	if err := ValidateSourcePaths(req.Sources); err != nil {
		return err
	}
	expected := make(map[string]bool)
	var total int64
	for _, file := range req.Sources {
		clean, err := safefs.Clean(file.Path)
		if err != nil || clean != file.Path || expected[file.Path] || strings.HasPrefix(file.Path, ".latexmk-build/") ||
			strings.HasPrefix(file.Path, ".latexmk-home/") {
			return errors.New("invalid source manifest")
		}
		expected[file.Path] = true
		total += file.Size
		if file.Size < 0 || total > req.MaxSourceBytes || len(expected) > req.MaxFiles {
			return errors.New("source manifest exceeds limit")
		}
		f, err := sourceFS.OpenRegular(file.Path)
		if err != nil {
			return err
		}
		err = errors.Join(safefs.CopyVerified(io.Discard, f, file.Size, file.SHA256), f.Close())
		if err != nil {
			return err
		}
	}
	if err := fs.WalkDir(sourceFS.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !expected[name] {
			return errors.New("undeclared source file")
		}
		return nil
	}); err != nil {
		return err
	}
	build := filepath.Join(project, ".latexmk-build")
	if err := ensureDir(build); err != nil {
		return err
	}
	if req.Warm {
		f, err := scoped.OpenRegular("checkpoint.tar.gz")
		if err != nil {
			return err
		}
		_, extractErr := projectarchive.ExtractTarGz(
			f,
			build,
			projectarchive.Limits{MaxBytes: req.MaxStateBytes, MaxFiles: req.MaxFiles},
		)
		if err := errors.Join(extractErr, f.Close()); err != nil {
			return err
		}
	}
	// Sources are disposable copies and made read-only before TeX runs. Generated
	// files can only use the separate writable build/home trees under this root.
	if err := ensureDir(filepath.Join(project, ".latexmk-home", ".texlive-var")); err != nil {
		return err
	}
	if err := ensureDir(filepath.Join(project, ".latexmk-home", ".texlive-config")); err != nil {
		return err
	}
	for name := range expected {
		if err := sourceFS.Chmod(name, 0444); err != nil {
			return err
		}
	}
	if err := fs.WalkDir(sourceFS.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == ".latexmk-build" || name == ".latexmk-home" {
			return fs.SkipDir
		}
		if d.IsDir() {
			return sourceFS.Chmod(name, 0555)
		}
		return nil
	}); err != nil {
		return err
	}
	cfg := config.Config{
		Engines:               []string{req.Request.Engine},
		MaxConcurrentCompiles: 1,
		CompileTimeout:        time.Duration(req.TimeoutMS) * time.Millisecond,
		MaxLogBytes:           req.MaxLogBytes,
		MaxArtifactBytes:      req.MaxArtifactBytes,
		CompileCacheRetention: time.Hour,
		MaxCompileCacheBytes:  req.MaxStateBytes,
	}
	runner := compile.NewRunner(cfg)
	result := runner.RunWithOptions(
		ctx,
		project,
		req.Request,
		req.RequestID,
		compile.RunOptions{BuildDirectory: ".latexmk-build", PreserveRecorder: req.Warm},
	)
	result.Result.WorkspaceReuse = req.Warm
	responsePath := filepath.Join(root, "result.tar.gz")
	if err := resultarchive.Write(responsePath, result); err != nil {
		return err
	}
	response, err := describeFile(root, "result.tar.gz", maxBytes)
	if err != nil {
		return err
	}
	members := []archiveMember{{name: "result.tar.gz", file: response}}
	if result.Result.Success && req.MaxStateBytes > 0 {
		// A successful no-op retains recorder and final output files from the previous
		// verified state; failure never exports a partially mutated checkpoint.
		state, err := collectState(build, req.MaxFiles, req.MaxStateBytes)
		if err == nil {
			checkpointPath := filepath.Join(root, "checkpoint.tar.gz")
			if err := writeArchiveFile(checkpointPath, state, req.MaxStateBytes+(1<<20)); err == nil {
				checkpoint, err := describeFile(root, "checkpoint.tar.gz", req.MaxStateBytes+(1<<20))
				if err == nil {
					members = append(members, archiveMember{name: "checkpoint.tar.gz", file: checkpoint})
				}
			}
		}
	}
	return writeArchive(output, members)
}

func hashReader(r io.Reader, max int64) (string, int64, error) {
	if max < 0 || max == int64(^uint64(0)>>1) {
		return "", 0, safefs.ErrLimit
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(r, max+1))
	if err == nil && size > max {
		err = safefs.ErrLimit
	}
	return hex.EncodeToString(hash.Sum(nil)), size, err
}

func workerError(err error) error { return fmt.Errorf("isolated compiler: %w", err) }
