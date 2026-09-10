// Package client communicates with the remote compiler and verifies downloaded artifacts.
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
	"sort"
	"strings"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

type Client struct {
	IgnoreFiles      []string
	DenyFiles        []string
	UnmatchedGlob    string
	BaseURL          string
	Token            string
	HTTP             *http.Client
	UserAgent        string
	ProjectRoot      string
	Exclude          []string
	RespectGitIgnore bool
	UploadMode       string
	ManifestFile     string
	IncludeFiles     []string
	ProjectID        string
}

type CompileOutput struct {
	Result   protocol.CompileResult
	Stdout   []byte
	Stderr   []byte
	Warnings []string
}

type StartCompileOutput struct {
	Job      protocol.Job
	Warnings []string
}

// HTTPError preserves the status code without exposing request credentials.
// CLI and MCP adapters use it to produce stable machine-readable errors.
type HTTPError struct {
	StatusCode int
	Status     string
	Message    string
}

type CapabilityError struct {
	Capability string
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("server does not support %s", e.Capability)
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server returned %s", e.Status)
	}
	return fmt.Sprintf("server returned %s: %s", e.Status, e.Message)
}

const (
	maxNeedsFileRounds = 3
	maxNeedsFiles      = 64
	maxNeedsFileBytes  = 64 << 20
)

func New(baseURL, token string, timeout time.Duration, insecure bool) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Opaque != "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return nil, errors.New("server URL must be an absolute http(s) URL without credentials, query, or fragment")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecure,
	} // TLS verification can be disabled explicitly by the user.
	return &Client{
		BaseURL:          baseURL,
		Token:            token,
		HTTP:             &http.Client{Timeout: timeout, Transport: transport},
		UserAgent:        "latexmk-cli/0.3.1",
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readHTTPError(resp)
	}
	return nil
}

func (c *Client) CleanupProject(ctx context.Context, projectID, scope string) (protocol.CleanupReport, error) {
	return c.cleanupProject(ctx, projectID, scope, http.MethodGet, "")
}

// CleanupProjectWithPlan applies a server-issued preview only while its digest
// still describes the exact same targets.
func (c *Client) CleanupProjectWithPlan(
	ctx context.Context,
	projectID, scope, expectedDigest string,
) (protocol.CleanupReport, error) {
	decoded, err := hex.DecodeString(expectedDigest)
	if err != nil || len(decoded) != sha256.Size {
		return protocol.CleanupReport{}, errors.New("cleanup plan digest must be a 64-character SHA-256 digest")
	}
	return c.cleanupProject(ctx, projectID, scope, http.MethodDelete, expectedDigest)
}

func (c *Client) cleanupProject(
	ctx context.Context,
	projectID, scope, method, expectedDigest string,
) (protocol.CleanupReport, error) {
	var report protocol.CleanupReport
	if !validProjectID(projectID) {
		return report, errors.New("project ID is invalid")
	}
	if scope != "results" && scope != "snapshot" && scope != "project" && scope != "cache" {
		return report, errors.New("cleanup scope must be results, snapshot, cache, or project")
	}
	path := "/v1/projects/" + url.PathEscape(projectID) + "/cleanup?scope=" + url.QueryEscape(scope)
	if expectedDigest != "" {
		path += "&expectedDigest=" + url.QueryEscape(expectedDigest)
	}
	if err := c.jsonRequest(ctx, method, path, nil, &report); err != nil {
		return report, err
	}
	return report, nil
}

// ListJobs returns jobs in a stable newest-first order. The server accepts
// limits from 1 through 200.
func (c *Client) ListJobs(ctx context.Context, limit int) ([]protocol.Job, error) {
	if limit < 1 || limit > 200 {
		return nil, errors.New("job limit must be between 1 and 200")
	}
	var response struct {
		Jobs []protocol.Job `json:"jobs"`
	}
	if err := c.jsonRequest(
		ctx,
		http.MethodGet,
		"/v1/jobs?limit="+url.QueryEscape(fmt.Sprint(limit)),
		nil,
		&response,
	); err != nil {
		return nil, err
	}
	sort.Slice(response.Jobs, func(i, j int) bool {
		if response.Jobs[i].CreatedAt.Equal(response.Jobs[j].CreatedAt) {
			return response.Jobs[i].ID < response.Jobs[j].ID
		}
		return response.Jobs[i].CreatedAt.After(response.Jobs[j].CreatedAt)
	})
	return response.Jobs, nil
}

func (c *Client) GetJob(ctx context.Context, jobID string) (protocol.Job, error) {
	var job protocol.Job
	if strings.TrimSpace(jobID) == "" {
		return job, errors.New("job ID is required")
	}
	if err := c.jsonRequest(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(jobID), nil, &job); err != nil {
		return job, err
	}
	return job, nil
}

func (c *Client) CancelJob(ctx context.Context, jobID string) (protocol.Job, error) {
	var job protocol.Job
	if strings.TrimSpace(jobID) == "" {
		return job, errors.New("job ID is required")
	}
	if err := c.jsonRequest(ctx, http.MethodDelete, "/v1/jobs/"+url.PathEscape(jobID), nil, &job); err != nil {
		return job, err
	}
	return job, nil
}

func (c *Client) Compile(
	ctx context.Context,
	request protocol.CompileRequest,
	outputRoot string,
) (CompileOutput, error) {
	files, selectionWarnings, err := c.projectManifest(request.Entry, request.Engine)
	if err != nil {
		return CompileOutput{}, err
	}
	meta, err := c.Metadata(ctx)
	if err != nil {
		return CompileOutput{}, err
	}
	if request.Auxiliary.Server == "reuse" &&
		(!meta.Capabilities.CompileCache || !meta.Capabilities.QueuedJobs || !meta.Capabilities.IncrementalUpload) {
		return CompileOutput{}, &CapabilityError{Capability: "server compile cache"}
	}
	request.RecordInputs = meta.Capabilities.DependencyInputs
	if err := validateAuxiliaryCapability(request, meta); err != nil {
		return CompileOutput{}, err
	}
	request.DetectMissingFiles = meta.Capabilities.NeedsFiles && (c.UploadMode == "" || c.UploadMode == "auto")
	output, err := c.compileOnce(ctx, request, outputRoot, files, meta)
	warnings := append([]string(nil), selectionWarnings...)
	if err != nil {
		output.Warnings = append(output.Warnings, warnings...)
		return output, err
	}

	selected := make(map[string]struct{}, len(files))
	for _, file := range files {
		selected[file.Path] = struct{}{}
	}
	additional := make([]string, 0)
	totalAddedFiles := 0
	var totalAddedBytes int64
	for round := 0; request.DetectMissingFiles && !output.Result.Success && len(output.Result.NeedsFiles) > 0; round++ {
		if round >= maxNeedsFileRounds {
			warnings = append(warnings, fmt.Sprintf("missing-file retry stopped after %d rounds", maxNeedsFileRounds))
			break
		}
		candidates, _, manifestErr := c.policyManifest()
		if manifestErr != nil {
			warnings = append(warnings, "missing-file retry refused: "+manifestErr.Error())
			break
		}
		requestedFiles, resolveErr := dependency.ResolveRequestedFiles(output.Result.NeedsFiles, candidates)
		if resolveErr != nil {
			warnings = append(warnings, "missing-file retry refused: "+resolveErr.Error())
			break
		}
		newFiles := make([]projectarchive.File, 0, len(requestedFiles))
		for _, file := range requestedFiles {
			if _, exists := selected[file.Path]; !exists {
				newFiles = append(newFiles, file)
			}
		}
		if len(newFiles) == 0 {
			warnings = append(warnings, "missing-file retry stopped because the server requested no new allowed files")
			break
		}
		var newBytes int64
		for _, file := range newFiles {
			newBytes += file.Size
		}
		if totalAddedFiles+len(newFiles) > maxNeedsFiles || totalAddedBytes+newBytes > maxNeedsFileBytes {
			warnings = append(
				warnings,
				fmt.Sprintf(
					"missing-file retry refused: additions exceed %d files or %d bytes",
					maxNeedsFiles,
					maxNeedsFileBytes,
				),
			)
			break
		}
		paths := make([]string, 0, len(newFiles))
		for _, file := range newFiles {
			selected[file.Path] = struct{}{}
			additional = append(additional, file.Path)
			paths = append(paths, file.Path)
		}
		totalAddedFiles += len(newFiles)
		totalAddedBytes += newBytes
		retryFiles, retryWarnings, manifestErr := c.projectManifestWithAdditional(
			request.Entry,
			request.Engine,
			additional,
		)
		if manifestErr != nil {
			warnings = append(warnings, "missing-file retry refused: "+manifestErr.Error())
			break
		}
		warnings = append(warnings, retryWarnings...)
		warnings = append(
			warnings,
			"server reported missing files; creating a new immutable snapshot with: "+strings.Join(paths, ", "),
		)
		output, err = c.compileOnce(ctx, request, outputRoot, retryFiles, meta)
		if err != nil {
			output.Warnings = append(output.Warnings, warnings...)
			return output, err
		}
	}
	output.Warnings = append(output.Warnings, warnings...)
	if output.Result.Success && len(output.Result.InputFiles) > 0 {
		if cacheErr := dependency.SaveCachedInputs(
			c.ProjectRoot,
			request.Entry,
			request.Engine,
			output.Result.InputFiles,
		); cacheErr != nil {
			output.Warnings = append(output.Warnings, "could not update dependency cache: "+cacheErr.Error())
		}
	}
	return output, nil
}

// StartCompile uploads one validated manifest and returns after the server has
// committed it to an immutable queued job. It never polls or downloads a
// result, so missing-file retries are left to the caller.
func (c *Client) StartCompile(ctx context.Context, request protocol.CompileRequest) (StartCompileOutput, error) {
	files, warnings, err := c.projectManifest(request.Entry, request.Engine)
	if err != nil {
		return StartCompileOutput{}, err
	}
	meta, err := c.Metadata(ctx)
	if err != nil {
		return StartCompileOutput{}, err
	}
	if !meta.Capabilities.IncrementalUpload || !meta.Capabilities.QueuedJobs {
		return StartCompileOutput{}, &CapabilityError{Capability: "detached queued compilation"}
	}
	if request.Auxiliary.Server == "reuse" && !meta.Capabilities.CompileCache {
		return StartCompileOutput{}, &CapabilityError{Capability: "server compile cache"}
	}
	request.RecordInputs = meta.Capabilities.DependencyInputs
	if err := validateAuxiliaryCapability(request, meta); err != nil {
		return StartCompileOutput{}, err
	}
	request.DetectMissingFiles = meta.Capabilities.NeedsFiles && (c.UploadMode == "" || c.UploadMode == "auto")
	job, err := c.startQueued(ctx, request, files)
	if err != nil {
		return StartCompileOutput{Warnings: warnings}, err
	}
	return StartCompileOutput{Job: job, Warnings: warnings}, nil
}

func (c *Client) compileOnce(
	ctx context.Context,
	request protocol.CompileRequest,
	outputRoot string,
	files []projectarchive.File,
	meta protocol.Metadata,
) (CompileOutput, error) {
	var output CompileOutput
	var err error
	if meta.Capabilities.IncrementalUpload && meta.Capabilities.QueuedJobs {
		output, err = c.compileQueued(ctx, request, outputRoot, files)
	} else {
		if meta.ProtocolVersion == 1 {
			request.ProtocolVersion = 1
		}
		output, err = c.compileLegacy(ctx, request, outputRoot, files)
	}
	return output, err
}

func (c *Client) compileLegacy(
	ctx context.Context,
	request protocol.CompileRequest,
	outputRoot string,
	files []projectarchive.File,
) (CompileOutput, error) {
	var out CompileOutput
	if c.ProjectRoot == "" {
		return out, errors.New("project root is not configured")
	}
	bodyFile, contentType, err := c.makeMultipart(request, files)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return out, readHTTPError(resp)
	}
	if err := unpackResponseWithPolicy(resp.Body, outputRoot, &out, request, c.ProjectRoot); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) compileQueued(
	ctx context.Context,
	request protocol.CompileRequest,
	outputRoot string,
	files []projectarchive.File,
) (CompileOutput, error) {
	var out CompileOutput
	job, err := c.startQueued(ctx, request, files)
	if err != nil {
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
		job, err = c.GetJob(ctx, job.ID)
		if err != nil {
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
	defer func() { _ = resp.Body.Close() }()
	if err := unpackResponseWithPolicy(resp.Body, outputRoot, &out, request, c.ProjectRoot); err != nil {
		return out, err
	}
	// Cache publication happens after the immutable result archive is durable.
	if job.Result != nil {
		out.Result.CompileCache = job.Result.CompileCache
	}
	return out, nil
}

func (c *Client) startQueued(
	ctx context.Context,
	request protocol.CompileRequest,
	files []projectarchive.File,
) (protocol.Job, error) {
	var job protocol.Job
	if c.ProjectRoot == "" {
		return job, errors.New("project root is not configured")
	}
	projectID := c.ProjectID
	if projectID == "" {
		var err error
		projectID, err = ResolveProjectID(c.ProjectRoot, true)
		if err != nil {
			return job, err
		}
	}
	planRequest := protocol.UploadPlanRequest{
		ProjectID: projectID,
		Request:   request,
		Files:     make([]protocol.ProjectFile, 0, len(files)),
	}
	byDigest := make(map[string]projectarchive.File, len(files))
	for _, file := range files {
		planRequest.Files = append(
			planRequest.Files,
			protocol.ProjectFile{Path: file.Path, SHA256: file.SHA256, Size: file.Size},
		)
		if _, exists := byDigest[file.SHA256]; !exists {
			byDigest[file.SHA256] = file
		}
	}
	var plan protocol.UploadPlan
	if err := c.jsonRequest(ctx, http.MethodPost, "/v1/uploads/plans", planRequest, &plan); err != nil {
		return job, err
	}
	for _, digest := range plan.Missing {
		source, ok := byDigest[digest]
		if !ok {
			return job, fmt.Errorf("server requested digest absent from manifest: %s", digest)
		}
		if err := c.uploadBlob(ctx, plan.UploadID, digest, source); err != nil {
			return job, err
		}
	}
	if err := c.jsonRequest(
		ctx,
		http.MethodPost,
		"/v1/uploads/"+url.PathEscape(plan.UploadID)+"/commit",
		nil,
		&job,
	); err != nil {
		return job, err
	}
	return job, nil
}

func (c *Client) makeMultipart(request protocol.CompileRequest, files []projectarchive.File) (*os.File, string, error) {
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
	if err := projectarchive.CreateFiles(projectPart, files); err != nil {
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

func (c *Client) projectManifest(entry, engine string) ([]projectarchive.File, []string, error) {
	return c.projectManifestWithAdditional(entry, engine, nil)
}

// Manifest returns the exact files a new compile would select before any
// server-assisted missing-file retries. Callers must not add other files.
func (c *Client) Manifest(entry, engine string) ([]projectarchive.File, []string, error) {
	return c.projectManifest(entry, engine)
}

func (c *Client) policyManifest() ([]projectarchive.File, string, error) {
	if c.ProjectRoot == "" {
		return nil, "", errors.New("project root is not configured")
	}
	exclude := append([]string(nil), c.Exclude...)
	manifestPath := ""
	manifestFile := c.ManifestFile
	if manifestFile == "" && c.UploadMode == "manifest" && len(c.IncludeFiles) == 0 {
		for _, name := range []string{".latexmk-manifest", ".latexmk-files"} {
			if _, err := os.Lstat(filepath.Join(c.ProjectRoot, name)); err == nil {
				manifestFile = name
				break
			}
		}
	}
	if manifestFile != "" {
		var err error
		manifestPath, err = dependency.NormalizeExplicitManifestPath(manifestFile)
		if err != nil {
			return nil, "", fmt.Errorf("manifest path: %w", err)
		}
		exclude = append(exclude, manifestPath)
	}
	denyFiles := append([]string{}, c.DenyFiles...)
	if manifestPath != "" {
		denyFiles = append(denyFiles, filepath.Join(c.ProjectRoot, manifestPath))
	}
	candidates, _, err := projectarchive.Manifest(projectarchive.Options{
		Root:        c.ProjectRoot,
		IgnoreFiles: c.IgnoreFiles, DenyFiles: denyFiles, DeferHash: true,
		Exclude:          exclude,
		RespectGitIgnore: c.RespectGitIgnore,
		MaxFiles:         20_000,
		MaxBytes:         2 << 30,
	})
	if err != nil {
		return nil, "", fmt.Errorf("build project manifest: %w", err)
	}
	return candidates, manifestPath, nil
}

func (c *Client) projectManifestWithAdditional(
	entry, engine string,
	additional []string,
) ([]projectarchive.File, []string, error) {
	result, err := c.Selection(entry, engine, additional)
	if err != nil {
		return nil, nil, err
	}
	if !result.Resolved {
		message := "dependency discovery has unresolved references"
		if len(result.Diagnostics) > 0 {
			message += ": " + dependency.FormatDiagnostic(result.Diagnostics[0])
		}
		return nil, nil, fmt.Errorf("%s; inspect with 'latexmk files'", message)
	}
	var warnings []string
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Resolution != "" {
			warnings = append(warnings, dependency.FormatDiagnostic(diagnostic))
		}
	}
	return result.Files, warnings, nil
}

// Selection is shared by preview, compilation and watch. It never reads credentials.
func (c *Client) Selection(entry, engine string, additional []string) (dependency.Result, error) {
	return c.selectFiles(entry, engine, additional, true)
}

// SelectionPaths refreshes watch membership without rehashing every unchanged
// graphic or other binary input at each polling interval.
func (c *Client) SelectionPaths(entry, engine string) (dependency.Result, error) {
	return c.selectFiles(entry, engine, nil, false)
}

func (c *Client) selectFiles(entry, engine string, additional []string, hash bool) (dependency.Result, error) {
	candidates, manifestPath, err := c.policyManifest()
	if err != nil {
		return dependency.Result{}, err
	}
	var cached []string
	explicit := append([]string(nil), c.IncludeFiles...)
	// Server-requested additions are exact validated paths, never user patterns.
	for _, name := range additional {
		explicit = append(explicit, dependency.ExactPattern(name))
	}
	historyAvailable := false
	if c.UploadMode != "all" {
		manifestFiles, manifestErr := dependency.LoadExplicitManifest(c.ProjectRoot, manifestPath)
		if manifestErr != nil {
			return dependency.Result{}, fmt.Errorf("load explicit manifest: %w", manifestErr)
		}
		explicit = append(explicit, manifestFiles...)
	}
	if c.UploadMode == "auto" || c.UploadMode == "" {
		cached, historyAvailable, err = dependency.LoadCachedInputs(c.ProjectRoot, entry, engine)
		if err != nil {
			return dependency.Result{}, fmt.Errorf("load dependency cache: %w", err)
		}
	}
	result, err := dependency.SelectWithOptions(
		entry,
		candidates,
		dependency.SelectionOptions{
			Mode:             c.UploadMode,
			UnmatchedGlob:    c.UnmatchedGlob,
			ExplicitFiles:    explicit,
			CachedFiles:      cached,
			HistoryAvailable: historyAvailable,
		},
	)
	if err != nil {
		return dependency.Result{}, fmt.Errorf("select project dependencies: %w", err)
	}
	if hash {
		if err := projectarchive.HashSelected(result.Files, 2<<30); err != nil {
			return dependency.Result{}, err
		}
	}
	result.Stats = projectarchive.Stats{Files: len(result.Files)}
	for _, file := range result.Files {
		result.Stats.Bytes += file.Size
	}
	return result, nil
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
	defer func() { _ = resp.Body.Close() }()
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

func (c *Client) uploadBlob(ctx context.Context, uploadID, digest string, source projectarchive.File) error {
	f, err := projectarchive.OpenFile(source)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	resp, err := c.rawRequest(
		ctx,
		http.MethodPut,
		"/v1/uploads/"+url.PathEscape(uploadID)+"/blobs/"+url.PathEscape(digest),
		f,
		"application/octet-stream",
	)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) rawRequest(
	ctx context.Context,
	method, path string,
	body io.Reader,
	contentType string,
) (*http.Response, error) {
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
		defer func() { _ = resp.Body.Close() }()
		return nil, readHTTPError(resp)
	}
	return resp, nil
}

func (c *Client) decorate(req *http.Request) {
	req.Header.Set("Accept", "application/json, application/vnd.latexmk.result+tar.gz")
	req.Header.Set("User-Agent", c.UserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func unpackResponse(r io.Reader, outputRoot string, out *CompileOutput) error {
	return unpackResponseWithPolicy(
		r,
		outputRoot,
		out,
		protocol.CompileRequest{Auxiliary: protocol.AuxiliaryOptions{Local: "output"}},
		outputRoot,
	)
}

func unpackResponseWithPolicy(
	r io.Reader,
	outputRoot string,
	out *CompileOutput,
	request protocol.CompileRequest,
	root string,
) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open result gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
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
		if h.Typeflag != tar.TypeReg {
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
				if err := storeReturnedArtifact(root, outputRoot, request, artifact, tr); err != nil {
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
	if clean == "." || filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
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
	defer func() { _ = os.Remove(tmpName) }()
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
		return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &payload) == nil && payload.Error != "" {
		return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Message: payload.Error}
	}
	return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Message: string(b)}
}
