package compile

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/config"
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
	RelativePath string
	AbsolutePath string
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
	entry, err := safeWorkspacePath(workspace, req.Entry)
	if err != nil {
		return fmt.Errorf("invalid entry: %w", err)
	}
	st, err := os.Stat(entry)
	if err != nil {
		return fmt.Errorf("entry: %w", err)
	}
	if !st.Mode().IsRegular() {
		return errors.New("entry is not a regular file")
	}
	return nil
}

func (r *Runner) ValidateRequest(req api.CompileRequest) error {
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
	_, err := safeWorkspacePath("/workspace", req.Entry)
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
	_ = removeStaleRecorderFiles(workspace)
	args := commandArgs(req)
	cmd := exec.CommandContext(ctx, "latexmk", args...)
	cmd.Dir = workspace
	cmd.Env = sandboxEnvironment(workspace, req.ShellEscape)
	configureProcess(cmd)
	stdout := newCappedBuffer(r.Config.MaxLogBytes)
	stderr := newCappedBuffer(r.Config.MaxLogBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	if ctx.Err() != nil {
		terminateProcessTree(cmd)
		result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	result.ExitCode = exitCode(err)
	result.Success = err == nil && !result.TimedOut
	if err != nil {
		result.Error = err.Error()
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
	result.StdoutTruncated = stdout.Truncated()
	result.StderrTruncated = stderr.Truncated()
	result.DurationMS = time.Since(started).Milliseconds()
	return Output{Result: result, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Files: files}
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

func sandboxEnvironment(workspace string, shellEscape bool) []string {
	home := filepath.Join(workspace, ".latexmk-home")
	texmfVar := filepath.Join(home, ".texlive-var")
	texmfConfig := filepath.Join(home, ".texlive-config")
	_ = os.MkdirAll(texmfVar, 0o700)
	_ = os.MkdirAll(texmfConfig, 0o700)
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
	}
}

func removeStaleRecorderFiles(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".fls") {
			return os.Remove(path)
		}
		return nil
	})
}

func collectArtifacts(root string, req api.CompileRequest, maxBytes int64) ([]File, error) {
	candidates := map[string]struct{}{}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".fls") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err == nil {
			candidates[filepath.ToSlash(rel)] = struct{}{}
		}
		return parseFLS(root, path, candidates)
	})
	if walkErr != nil {
		return nil, walkErr
	}
	// Recorder files describe TeX outputs, but XeLaTeX's final PDF may be
	// produced later by xdvipdfmx and omitted from the .fls file. Always add
	// artifacts matching the effective job name as a second discovery path.
	stem := req.JobName
	if stem == "" {
		stem = strings.TrimSuffix(filepath.Base(req.Entry), filepath.Ext(req.Entry))
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), stem+".") && allowedArtifact(d.Name()) {
			if rel, relErr := filepath.Rel(root, path); relErr == nil {
				candidates[filepath.ToSlash(rel)] = struct{}{}
			}
		}
		return nil
	})
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
		abs, err := safeWorkspacePath(root, rel)
		if err != nil {
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		total += st.Size()
		if total > maxBytes {
			return nil, fmt.Errorf("artifacts exceed %d bytes", maxBytes)
		}
		hash, err := hashFile(abs)
		if err != nil {
			return nil, err
		}
		files = append(files, File{RelativePath: rel, AbsolutePath: abs, Size: st.Size(), SHA256: hash})
	}
	return files, nil
}

func parseFLS(root, flsPath string, candidates map[string]struct{}) error {
	f, err := os.Open(flsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, 16<<20))
	s.Buffer(make([]byte, 64<<10), 1<<20)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "OUTPUT ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "OUTPUT "))
		if value == "" {
			continue
		}
		abs := value
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, filepath.FromSlash(value))
		}
		abs, err = filepath.Abs(abs)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		candidates[filepath.ToSlash(rel)] = struct{}{}
	}
	return s.Err()
}

func collectRecordedInputs(root string) ([]string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace for input collection: %w", err)
	}
	inputs := make(map[string]struct{})
	err = filepath.WalkDir(rootAbs, func(filePath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".fls") {
			return nil
		}
		return parseRecordedInputs(rootResolved, filePath, inputs)
	})
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(inputs))
	for input := range inputs {
		paths = append(paths, input)
	}
	sort.Strings(paths)
	return paths, nil
}

func parseRecordedInputs(root, flsPath string, inputs map[string]struct{}) error {
	f, err := os.Open(flsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	pwd := root
	s := bufio.NewScanner(io.LimitReader(f, 16<<20))
	s.Buffer(make([]byte, 64<<10), 1<<20)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "PWD ") {
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "PWD "))
			if resolved, ok := recordedPath(root, root, candidate); ok {
				pwd = resolved
			}
			continue
		}
		if !strings.HasPrefix(line, "INPUT ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "INPUT "))
		resolved, ok := recordedPath(root, pwd, value)
		if !ok {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		inputs[filepath.ToSlash(rel)] = struct{}{}
	}
	return s.Err()
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

func safeWorkspacePath(root, rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, '\x00') || strings.Contains(rel, "\\") {
		return "", errors.New("empty or malformed path")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", err
	}
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	return abs, nil
}

func validJobName(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func newCappedBuffer(max int64) *cappedBuffer { return &cappedBuffer{max: max} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	remaining := b.max - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *cappedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *cappedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
