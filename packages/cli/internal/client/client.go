package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

type Client struct {
	BaseURL          string
	Token            string
	HTTP             *http.Client
	UserAgent        string
	ProjectRoot      string
	Exclude          []string
	RespectGitIgnore bool
}

type CompileOutput struct {
	Result protocol.CompileResult
	Stdout []byte
	Stderr []byte
}

func New(baseURL, token string, timeout time.Duration, insecure bool) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("server URL must be an absolute http(s) URL without credentials, query, or fragment")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec -- explicit user option
	return &Client{
		BaseURL:          baseURL,
		Token:            token,
		HTTP:             &http.Client{Timeout: timeout, Transport: transport},
		UserAgent:        "latexmk-cli/0.1.0",
		RespectGitIgnore: true,
	}, nil
}

func (c *Client) Metadata(ctx context.Context) (protocol.Metadata, error) {
	var meta protocol.Metadata
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/meta", nil)
	if err != nil {
		return meta, err
	}
	c.decorate(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return meta, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return meta, readHTTPError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return meta, fmt.Errorf("decode metadata: %w", err)
	}
	return meta, nil
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	c.decorate(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readHTTPError(resp)
	}
	return nil
}

func (c *Client) Compile(ctx context.Context, request protocol.CompileRequest, outputRoot string) (CompileOutput, error) {
	meta, err := c.Metadata(ctx)
	if err != nil {
		return CompileOutput{}, err
	}
	if meta.Capabilities.IncrementalUpload && meta.Capabilities.QueuedJobs {
		return c.compileQueued(ctx, request, outputRoot)
	}
	if meta.ProtocolVersion == 1 {
		request.ProtocolVersion = 1
	}
	return c.compileLegacy(ctx, request, outputRoot)
}

func (c *Client) compileLegacy(ctx context.Context, request protocol.CompileRequest, outputRoot string) (CompileOutput, error) {
	var out CompileOutput
	if c.ProjectRoot == "" {
		return out, errors.New("project root is not configured")
	}
	bodyFile, contentType, err := c.makeMultipart(request)
	if err != nil {
		return out, err
	}
	defer func() {
		name := bodyFile.Name()
		_ = bodyFile.Close()
		_ = os.Remove(name)
	}()
	st, err := bodyFile.Stat()
	if err != nil {
		return out, err
	}
	if _, err := bodyFile.Seek(0, io.SeekStart); err != nil {
		return out, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/compile", bodyFile)
	if err != nil {
		return out, err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", contentType)
	c.decorate(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return out, readHTTPError(resp)
	}
	if err := unpackResponse(resp.Body, outputRoot, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) compileQueued(ctx context.Context, request protocol.CompileRequest, outputRoot string) (CompileOutput, error) {
	var out CompileOutput
	if c.ProjectRoot == "" {
		return out, errors.New("project root is not configured")
	}
	files, _, err := projectarchive.Manifest(projectarchive.Options{
		Root: c.ProjectRoot, Exclude: c.Exclude, RespectGitIgnore: c.RespectGitIgnore, MaxFiles: 20_000, MaxBytes: 2 << 30,
	})
	if err != nil {
		return out, fmt.Errorf("build project manifest: %w", err)
	}
	projectID, err := stableProjectID(c.ProjectRoot)
	if err != nil {
		return out, err
	}
	planRequest := protocol.UploadPlanRequest{ProjectID: projectID, Request: request, Files: make([]protocol.ProjectFile, 0, len(files))}
	byDigest := make(map[string]string, len(files))
	for _, file := range files {
		planRequest.Files = append(planRequest.Files, protocol.ProjectFile{Path: file.Path, SHA256: file.SHA256, Size: file.Size})
		if _, exists := byDigest[file.SHA256]; !exists {
			byDigest[file.SHA256] = file.Source
		}
	}
	var plan protocol.UploadPlan
	if err := c.jsonRequest(ctx, http.MethodPost, "/v1/uploads/plans", planRequest, &plan); err != nil {
		return out, err
	}
	for _, digest := range plan.Missing {
		source, ok := byDigest[digest]
		if !ok {
			return out, fmt.Errorf("server requested digest absent from manifest: %s", digest)
		}
		if err := c.uploadBlob(ctx, plan.UploadID, digest, source); err != nil {
			return out, err
		}
	}
	var job protocol.Job
	if err := c.jsonRequest(ctx, http.MethodPost, "/v1/uploads/"+url.PathEscape(plan.UploadID)+"/commit", nil, &job); err != nil {
		return out, err
	}
	for {
		if job.Status == "succeeded" || job.Status == "failed" || job.Status == "cancelled" {
			break
		}
		timer := time.NewTimer(350 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
		if err := c.jsonRequest(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(job.ID), nil, &job); err != nil {
			return out, err
		}
	}
	if job.Status == "cancelled" {
		return out, errors.New("remote compile job was cancelled")
	}
	resp, err := c.rawRequest(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(job.ID)+"/result", nil, "")
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := unpackResponse(resp.Body, outputRoot, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) makeMultipart(request protocol.CompileRequest) (*os.File, string, error) {
	f, err := os.CreateTemp("", "latexmk-request-*.multipart")
	if err != nil {
		return nil, "", err
	}
	cleanup := func(e error) (*os.File, string, error) {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		return nil, "", e
	}
	mw := multipart.NewWriter(f)
	requestPart, err := mw.CreateFormField("request")
	if err != nil {
		return cleanup(err)
	}
	if err := json.NewEncoder(requestPart).Encode(request); err != nil {
		return cleanup(err)
	}
	projectPart, err := mw.CreateFormFile("project", "project.tar.gz")
	if err != nil {
		return cleanup(err)
	}
	if _, err := projectarchive.Create(projectPart, projectarchive.Options{
		Root:             c.ProjectRoot,
		Exclude:          c.Exclude,
		RespectGitIgnore: c.RespectGitIgnore,
		MaxFiles:         20_000,
		MaxBytes:         2 << 30,
	}); err != nil {
		return cleanup(fmt.Errorf("archive project: %w", err))
	}
	if err := mw.Close(); err != nil {
		return cleanup(err)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err)
	}
	return f, mw.FormDataContentType(), nil
}

func (c *Client) jsonRequest(ctx context.Context, method, path string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	resp, err := c.rawRequest(ctx, method, path, reader, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if output == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (c *Client) uploadBlob(ctx context.Context, uploadID, digest, source string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	resp, err := c.rawRequest(ctx, http.MethodPut, "/v1/uploads/"+url.PathEscape(uploadID)+"/blobs/"+url.PathEscape(digest), f, "application/octet-stream")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) rawRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.decorate(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, readHTTPError(resp)
	}
	return resp, nil
}

func stableProjectID(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(filepath.Clean(resolved)))
	return "project-" + hex.EncodeToString(digest[:16]), nil
}

func (c *Client) decorate(req *http.Request) {
	req.Header.Set("Accept", "application/json, application/vnd.latexmk.result+tar.gz")
	req.Header.Set("User-Agent", c.UserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func unpackResponse(r io.Reader, outputRoot string, out *CompileOutput) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open result gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var resultSeen bool
	var stdoutSeen bool
	var stderrSeen bool
	expected := map[string]protocol.Artifact{}
	seen := map[string]bool{}
	var totalBytes int64
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read result tar: %w", err)
		}
		entries++
		if entries > 20_000 {
			return errors.New("result archive contains too many entries")
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return fmt.Errorf("unexpected result entry type for %q", h.Name)
		}
		if h.Size < 0 || h.Size > 512<<20 {
			return fmt.Errorf("result entry %q is too large", h.Name)
		}
		totalBytes += h.Size
		if totalBytes > 1<<30 {
			return errors.New("result archive expands beyond 1 GiB")
		}
		switch h.Name {
		case "result.json":
			if resultSeen {
				return errors.New("result archive contains duplicate result.json")
			}
			if h.Size > 1<<20 {
				return errors.New("result.json is too large")
			}
			payload, err := io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return fmt.Errorf("read result: %w", err)
			}
			decoder := json.NewDecoder(bytes.NewReader(payload))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&out.Result); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			if decoder.Decode(&struct{}{}) != io.EOF {
				return errors.New("result.json contains trailing data")
			}
			if out.Result.ProtocolVersion != 1 && out.Result.ProtocolVersion != protocol.Version {
				return fmt.Errorf("unsupported response protocol version %d", out.Result.ProtocolVersion)
			}
			resultSeen = true
			for _, artifact := range out.Result.Artifacts {
				if artifact.Path == "" || artifact.Size < 0 || artifact.SHA256 == "" {
					return errors.New("result contains invalid artifact metadata")
				}
				if _, duplicate := expected[artifact.Path]; duplicate {
					return fmt.Errorf("result declares duplicate artifact %q", artifact.Path)
				}
				expected[artifact.Path] = artifact
			}
		case "stdout.log":
			if stdoutSeen {
				return errors.New("result archive contains duplicate stdout.log")
			}
			if h.Size > 16<<20 {
				return errors.New("stdout.log is too large")
			}
			out.Stdout, err = io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return err
			}
			stdoutSeen = true
		case "stderr.log":
			if stderrSeen {
				return errors.New("result archive contains duplicate stderr.log")
			}
			if h.Size > 16<<20 {
				return errors.New("stderr.log is too large")
			}
			out.Stderr, err = io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return err
			}
			stderrSeen = true
		default:
			if strings.HasPrefix(h.Name, "artifacts/") {
				if !resultSeen {
					return errors.New("artifact appeared before result.json")
				}
				rel := strings.TrimPrefix(h.Name, "artifacts/")
				artifact, ok := expected[rel]
				if !ok {
					return fmt.Errorf("server returned undeclared artifact %q", rel)
				}
				if seen[rel] {
					return fmt.Errorf("server returned duplicate artifact %q", rel)
				}
				if artifact.Size != h.Size {
					return fmt.Errorf("artifact %q size mismatch", rel)
				}
				if err := writeArtifact(outputRoot, rel, tr, h.Size, artifact.SHA256); err != nil {
					return err
				}
				seen[rel] = true
				continue
			}
			return fmt.Errorf("unexpected result entry %q", h.Name)
		}
	}
	if !resultSeen {
		return errors.New("server response did not contain result.json")
	}
	for rel := range expected {
		if !seen[rel] {
			return fmt.Errorf("server omitted declared artifact %q", rel)
		}
	}
	return nil
}

func writeArtifact(root, rel string, r io.Reader, size int64, expectedSHA256 string) error {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe artifact path %q", rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootAbs, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("resolve output root: %w", err)
	}
	dstAbs := filepath.Join(rootAbs, clean)
	if dstAbs != rootAbs && !strings.HasPrefix(dstAbs, rootAbs+string(filepath.Separator)) {
		return fmt.Errorf("artifact escaped output root: %q", rel)
	}
	parent, err := ensureSafeParent(rootAbs, filepath.Dir(clean))
	if err != nil {
		return fmt.Errorf("prepare artifact %q: %w", rel, err)
	}
	dstAbs = filepath.Join(parent, filepath.Base(clean))
	if info, err := os.Lstat(dstAbs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact destination is a symbolic link: %q", rel)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".latexmk-artifact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(tmp, hash), r, size)
	if copyErr != nil {
		_ = tmp.Close()
		return copyErr
	}
	if n != size {
		_ = tmp.Close()
		return io.ErrUnexpectedEOF
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(digest, expectedSHA256) {
		return fmt.Errorf("artifact %q SHA-256 mismatch", rel)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(dstAbs)
	}
	if err := os.Rename(tmpName, dstAbs); err != nil {
		return err
	}
	return nil
}

func ensureSafeParent(root, relativeDir string) (string, error) {
	current := root
	if relativeDir == "." || relativeDir == "" {
		return current, nil
	}
	for _, part := range strings.Split(relativeDir, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe directory component %q", part)
		}
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("symbolic link in output path: %q", part)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("non-directory in output path: %q", part)
			}
		case os.IsNotExist(err):
			if err := os.Mkdir(next, 0o755); err != nil {
				return "", err
			}
		default:
			return "", err
		}
		current = next
	}
	return current, nil
}

func readHTTPError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("server returned %s: %s", resp.Status, payload.Error)
	}
	return fmt.Errorf("server returned %s: %s", resp.Status, string(b))
}
