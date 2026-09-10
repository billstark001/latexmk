// Package compile validates requests, runs TeX tools, and collects compilation output.
package compile

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/platform/process"
	"github.com/billstark001/latexmk/packages/server/internal/platform/safefs"
)

type Runner struct {
	Config config.Config
	sem    chan struct{}
}

type Output struct {
	Result api.CompileResult
	Stdout []byte
	Stderr []byte
	Files  []File
}

type File struct {
	Workspace    string
	RelativePath string
	Size         int64
	SHA256       string
}

func NewRunner(cfg config.Config) *Runner {
	return &Runner{Config: cfg, sem: make(chan struct{}, cfg.MaxConcurrentCompiles)}
}

func (r *Runner) Validate(workspace string, req api.CompileRequest) error {
	if err := r.ValidateRequest(req); err != nil {
		return err
	}
	fs, err := safefs.Open(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = fs.Close() }()
	entry, err := fs.OpenRegular(req.Entry)
	if err != nil {
		return fmt.Errorf("entry: %w", err)
	}
	return entry.Close()
}

func (r *Runner) ValidateRequest(req api.CompileRequest) error {
	if req.Auxiliary.Local != "" && req.Auxiliary.Local != "none" && req.Auxiliary.Local != "cache" &&
		req.Auxiliary.Local != "output" {
		return errors.New("auxiliary.local must be none, cache, or output")
	}
	if req.Auxiliary.ServerTTL != "" {
		ttl, err := time.ParseDuration(req.Auxiliary.ServerTTL)
		if err != nil || ttl <= 0 {
			return errors.New("auxiliary.serverTTL must be a positive duration")
		}
	}

	if req.Auxiliary.Server != "" && req.Auxiliary.Server != "none" && req.Auxiliary.Server != "reuse" &&
		req.Auxiliary.Server != "retain" {
		return errors.New("auxiliary.server must be none, retain, or reuse")
	}
	if req.Auxiliary.Server == "reuse" &&
		(req.ProtocolVersion != api.ProtocolVersion || r.Config.CompileCacheRetention <= 0 || r.Config.MaxCompileCacheBytes <= 0) {
		return errors.New("server compile cache is not enabled for this request")
	}
	if req.ProtocolVersion != 1 && req.ProtocolVersion != api.ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", req.ProtocolVersion)
	}
	if !r.Config.EngineAllowed(req.Engine) {
		return fmt.Errorf("engine %q is not enabled", req.Engine)
	}
	switch req.Interaction {
	case "batchmode", "nonstopmode", "scrollmode", "errorstopmode":
	default:
		return fmt.Errorf("unsupported interaction mode %q", req.Interaction)
	}
	if req.ShellEscape && !r.Config.AllowShellEscape {
		return errors.New("shell escape is disabled by server policy")
	}
	if req.JobName != "" && !validJobName(req.JobName) {
		return errors.New("jobName may contain only letters, digits, dot, underscore, and hyphen")
	}
	_, err := safefs.Clean(req.Entry)
	if err != nil {
		return fmt.Errorf("invalid entry: %w", err)
	}
	return nil
}

func (r *Runner) Run(parent context.Context, workspace string, req api.CompileRequest, requestID string) Output {
	started := time.Now()
	result := api.CompileResult{
		ProtocolVersion: api.ProtocolVersion,
		RequestID:       requestID,
		Entry:           req.Entry,
		Engine:          req.Engine,
		ExitCode:        -1,
	}
	if err := r.Validate(workspace, req); err != nil {
		result.Error = err.Error()
		result.DurationMS = time.Since(started).Milliseconds()
		return Output{Result: result}
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-parent.Done():
		result.Error = parent.Err().Error()
		result.TimedOut = errors.Is(parent.Err(), context.DeadlineExceeded)
		result.DurationMS = time.Since(started).Milliseconds()
		return Output{Result: result}
	}

	ctx, cancel := context.WithTimeout(parent, r.Config.CompileTimeout)
	defer cancel()
	if err := removeStaleRecorderFiles(workspace); err != nil {
		result.Error = err.Error()
		result.DurationMS = time.Since(started).Milliseconds()
		return Output{Result: result}
	}
	args := commandArgs(req)
	env, err := sandboxEnvironment(workspace, req.ShellEscape)
	if err != nil {
		result.Error = err.Error()
		result.DurationMS = time.Since(started).Milliseconds()
		return Output{Result: result}
	}
	executed := process.Run(
		ctx,
		process.Spec{Name: "latexmk", Args: args, Dir: workspace, Env: env, MaxOutputBytes: r.Config.MaxLogBytes},
	)
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	result.ExitCode = executed.ExitCode
	result.Success = executed.Err == nil && !result.TimedOut
	if executed.Err != nil {
		result.Error = executed.Err.Error()
	}
	if result.TimedOut {
		result.Error = "compilation timed out"
	}

	files, collectErr := collectArtifacts(workspace, req, r.Config.MaxArtifactBytes)
	if collectErr != nil {
		if result.Error == "" {
			result.Error = collectErr.Error()
		} else {
			result.Error += "; artifact collection: " + collectErr.Error()
		}
		result.Success = false
	}
	for _, f := range files {
		result.Artifacts = append(result.Artifacts, api.Artifact{Path: f.RelativePath, Size: f.Size, SHA256: f.SHA256})
	}
	if req.RecordInputs {
		inputFiles, inputErr := collectRecordedInputs(workspace)
		if inputErr != nil {
			if result.Error == "" {
				result.Error = inputErr.Error()
			} else {
				result.Error += "; input collection: " + inputErr.Error()
			}
			result.Success = false
		} else {
			result.InputFiles = inputFiles
		}
	}
	if req.DetectMissingFiles && !result.Success {
		result.NeedsFiles = detectMissingFiles(executed.Stdout, executed.Stderr, files)
	}
	result.StdoutTruncated = executed.StdoutTruncated
	result.StderrTruncated = executed.StderrTruncated
	result.DurationMS = time.Since(started).Milliseconds()
	return Output{Result: result, Stdout: executed.Stdout, Stderr: executed.Stderr, Files: files}
}

func commandArgs(req api.CompileRequest) []string {
	args := []string{"-norc"}
	switch req.Engine {
	case "xelatex":
		args = append(args, "-xelatex")
	case "lualatex":
		args = append(args, "-lualatex", "-pdflualatex=lualatex --safer --nosocket %O %S")
	case "pdflatex":
		args = append(args, "-pdf")
	}
	args = append(args, "-interaction="+req.Interaction, "-recorder")
	if req.Synctex {
		args = append(args, "-synctex=1")
	} else {
		args = append(args, "-synctex=0")
	}
	if req.HaltOnError {
		args = append(args, "-halt-on-error")
	}
	if req.FileLineError {
		args = append(args, "-file-line-error")
	}
	if req.ShellEscape {
		args = append(args, "-shell-escape")
	} else {
		args = append(args, "-no-shell-escape")
	}
	if req.JobName != "" {
		args = append(args, "-jobname="+req.JobName)
	}
	if req.Force {
		args = append(args, "-g")
	}
	if req.Quiet {
		args = append(args, "-silent")
	}
	args = append(args, req.Entry)
	return args
}

func sandboxEnvironment(workspace string, shellEscape bool) ([]string, error) {
	home := filepath.Join(workspace, ".latexmk-home")
	texmfVar := filepath.Join(home, ".texlive-var")
	texmfConfig := filepath.Join(home, ".texlive-config")
	fs, err := safefs.Open(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fs.Close() }()
	for _, dir := range []string{".latexmk-home/.texlive-var", ".latexmk-home/.texlive-config"} {
		if err := fs.MakeDirs(dir, 0o700); err != nil {
			return nil, err
		}
	}
	shell := "f"
	if shellEscape {
		shell = "t"
	}
	// Do not inherit the server process environment. In a PaaS that can contain
	// cloud credentials, proxy settings, or TeX search-path overrides; TeX and
	// any accidentally enabled child process must only see this small whitelist.
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TZ=UTC",
		"HOME=" + home,
		"TEXMFHOME=" + filepath.Join(home, "texmf"),
		"TEXMFVAR=" + texmfVar,
		"TEXMFCONFIG=" + texmfConfig,
		"openin_any=p",
		"openout_any=p",
		"shell_escape=" + shell,
	}, nil
}

func removeStaleRecorderFiles(root string) error {
	scoped, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	return fs.WalkDir(scoped.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".fls") {
			return scoped.Remove(name)
		}
		return nil
	})
}

func collectArtifacts(root string, req api.CompileRequest, maxBytes int64) ([]File, error) {
	scoped, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()
	candidates, err := recorderPaths(scoped, "OUTPUT")
	if err != nil {
		return nil, err
	}
	stem := req.JobName
	if stem == "" {
		stem = strings.TrimSuffix(filepath.Base(req.Entry), filepath.Ext(req.Entry))
	}
	// xdvipdfmx may produce the final PDF after the recorder has closed.
	err = fs.WalkDir(scoped.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() &&
			(strings.HasSuffix(strings.ToLower(d.Name()), ".fls") || strings.HasPrefix(d.Name(), stem+".")) &&
			allowedArtifact(name) {
			candidates[name] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(candidates))
	for rel := range candidates {
		if allowedArtifact(rel) {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	files := make([]File, 0, len(paths))
	var total int64
	for _, rel := range paths {
		f, err := scoped.OpenRegular(rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		hash, size, readErr := digestFile(f, maxBytes-total)
		err = errors.Join(readErr, f.Close())
		if err != nil {
			return nil, err
		}
		total += size
		files = append(
			files,
			File{
				Workspace:    root,
				RelativePath: rel,
				Size:         size,
				SHA256:       hash,
			},
		)
	}
	return files, nil
}

func digestFile(f *os.File, max int64) (string, int64, error) {
	if max < 0 || max == int64(^uint64(0)>>1) {
		return "", 0, safefs.ErrLimit
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, max+1))
	if err != nil {
		return "", 0, err
	}
	if n > max {
		return "", 0, safefs.ErrLimit
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func collectRecordedInputs(root string) ([]string, error) {
	scoped, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()
	candidates, err := recorderPaths(scoped, "INPUT")
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(candidates))
	for rel := range candidates {
		f, err := scoped.OpenRegular(rel)
		if err != nil {
			continue
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	return paths, nil
}

// recorderPaths shares bounded parsing and PWD handling for input/output records.
func recorderPaths(scoped *safefs.Root, kind string) (map[string]struct{}, error) {
	root, err := filepath.Abs(scoped.Name())
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]struct{})
	err = fs.WalkDir(scoped.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".fls") {
			return nil
		}
		data, err := scoped.ReadLimited(name, 16<<20)
		if err != nil {
			return err
		}
		pwd := root
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			tag, value, ok := strings.Cut(scanner.Text(), " ")
			if !ok || (tag != kind && tag != "PWD") {
				continue
			}
			value = strings.TrimSpace(value)
			if canonical != root &&
				(value == canonical || strings.HasPrefix(value, canonical+string(filepath.Separator))) {
				value = root + strings.TrimPrefix(value, canonical)
			}
			absolute, ok := recordedPath(root, pwd, value)
			if !ok {
				continue
			}
			if tag == "PWD" {
				pwd = absolute
				continue
			}
			rel, err := filepath.Rel(root, absolute)
			if err != nil {
				continue
			}
			clean, err := safefs.Clean(filepath.ToSlash(rel))
			if err == nil {
				paths[clean] = struct{}{}
			}
		}
		return scanner.Err()
	})
	return paths, err
}

// Open reopens an artifact through its owning workspace, never through a raw
// compiler-controlled absolute path.
func (f File) Open() (*os.File, error) {
	root, name := f.Workspace, f.RelativePath
	if root == "" {
		return nil, errors.New("artifact has no owning workspace")
	}
	scoped, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	file, err := scoped.OpenRegular(name)
	closeErr := scoped.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, file.Close())
	}
	return file, nil
}

func recordedPath(root, base, value string) (string, bool) {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	candidate := filepath.FromSlash(value)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(base, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return candidate, true
}

func allowedArtifact(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	for _, suffix := range []string{
		".pdf", ".log", ".aux", ".bbl", ".bcf", ".blg", ".fdb_latexmk", ".fls", ".out", ".run.xml", ".synctex.gz", ".toc", ".xdv", ".lof", ".lot", ".idx", ".ind", ".ilg", ".nav", ".snm", ".vrb", ".glg", ".glo", ".gls", ".ist",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func validJobName(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' ||
			r == '-' {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}
