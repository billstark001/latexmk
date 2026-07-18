package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

type ResultStateError struct {
	Status string
}

type ArtifactNotFoundError struct {
	ID string
}

func (e *ArtifactNotFoundError) Error() string {
	return fmt.Sprintf("artifact ID %s was not found in this job", e.ID)
}

func (e *ResultStateError) Error() string {
	switch e.Status {
	case "queued", "running":
		return fmt.Sprintf("job result is not ready while status is %s", e.Status)
	default:
		return fmt.Sprintf("job result is unavailable while status is %s", e.Status)
	}
}

type ArtifactInfo struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	MIMEType string `json:"mimeType"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type DownloadedArtifact struct {
	ArtifactInfo
	LocalPath string `json:"localPath"`
}

type LogEntry struct {
	Source        string `json:"source"`
	Path          string `json:"path"`
	Content       string `json:"content"`
	TotalBytes    int64  `json:"totalBytes"`
	ReturnedBytes int64  `json:"returnedBytes"`
	Truncated     bool   `json:"truncated"`
}

type LogsOutput struct {
	JobID       string     `json:"jobId"`
	Source      string     `json:"source"`
	TailLines   int        `json:"tailLines"`
	MaxBytes    int64      `json:"maxBytes"`
	Returned    int64      `json:"returnedBytes"`
	Entries     []LogEntry `json:"entries"`
	ArchiveDone bool       `json:"archiveComplete"`
}

func (c *Client) ListArtifacts(ctx context.Context, jobID string) ([]ArtifactInfo, error) {
	job, err := c.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := requireJobResult(job); err != nil {
		return nil, err
	}
	artifacts := make([]ArtifactInfo, 0, len(job.Result.Artifacts))
	ids := make(map[string]string, len(job.Result.Artifacts))
	paths := make(map[string]struct{}, len(job.Result.Artifacts))
	for _, artifact := range job.Result.Artifacts {
		if err := validateArtifactMetadata(artifact); err != nil {
			return nil, err
		}
		if _, duplicate := paths[artifact.Path]; duplicate {
			return nil, fmt.Errorf("job declares duplicate artifact %q", artifact.Path)
		}
		paths[artifact.Path] = struct{}{}
		id := artifactID(artifact.Path)
		if previous, exists := ids[id]; exists && previous != artifact.Path {
			return nil, errors.New("artifact ID collision")
		}
		ids[id] = artifact.Path
		artifacts = append(artifacts, ArtifactInfo{
			ID: id, Path: artifact.Path, MIMEType: artifactMIMEType(artifact.Path),
			Size: artifact.Size, SHA256: artifact.SHA256,
		})
	}
	return artifacts, nil
}

func (c *Client) DownloadArtifact(ctx context.Context, jobID, artifactIDValue, outputRoot string) (DownloadedArtifact, error) {
	artifacts, err := c.ListArtifacts(ctx, jobID)
	if err != nil {
		return DownloadedArtifact{}, err
	}
	var selected ArtifactInfo
	for _, artifact := range artifacts {
		if artifact.ID == artifactIDValue {
			selected = artifact
			break
		}
	}
	if selected.ID == "" {
		return DownloadedArtifact{}, &ArtifactNotFoundError{ID: artifactIDValue}
	}
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return DownloadedArtifact{}, fmt.Errorf("create output directory: %w", err)
	}
	resp, err := c.resultResponse(ctx, jobID)
	if err != nil {
		return DownloadedArtifact{}, err
	}
	defer resp.Body.Close()
	if err := extractSelectedArtifact(resp.Body, outputRoot, selected); err != nil {
		return DownloadedArtifact{}, err
	}
	rootAbs, err := filepath.Abs(outputRoot)
	if err != nil {
		return DownloadedArtifact{}, err
	}
	return DownloadedArtifact{ArtifactInfo: selected, LocalPath: filepath.Join(rootAbs, filepath.FromSlash(selected.Path))}, nil
}

func (c *Client) Logs(ctx context.Context, jobID, source string, tailLines int, maxBytes int64) (LogsOutput, error) {
	if source != "all" && source != "stdout" && source != "stderr" && source != "compiler" {
		return LogsOutput{}, errors.New("log source must be all, stdout, stderr, or compiler")
	}
	if tailLines < 1 || tailLines > 10_000 {
		return LogsOutput{}, errors.New("log tail must be between 1 and 10000 lines")
	}
	if maxBytes < 1 || maxBytes > 4<<20 {
		return LogsOutput{}, errors.New("log max bytes must be between 1 and 4194304")
	}
	job, err := c.GetJob(ctx, jobID)
	if err != nil {
		return LogsOutput{}, err
	}
	if err := requireJobResult(job); err != nil {
		return LogsOutput{}, err
	}
	declared := make(map[string]protocol.Artifact, len(job.Result.Artifacts))
	selectedLogs := 0
	if source == "all" || source == "stdout" {
		selectedLogs++
	}
	if source == "all" || source == "stderr" {
		selectedLogs++
	}
	for _, artifact := range job.Result.Artifacts {
		if err := validateArtifactMetadata(artifact); err != nil {
			return LogsOutput{}, err
		}
		if _, duplicate := declared[artifact.Path]; duplicate {
			return LogsOutput{}, fmt.Errorf("job declares duplicate artifact %q", artifact.Path)
		}
		declared[artifact.Path] = artifact
		if (source == "all" || source == "compiler") && strings.EqualFold(filepath.Ext(artifact.Path), ".log") {
			selectedLogs++
		}
	}
	resp, err := c.resultResponse(ctx, jobID)
	if err != nil {
		return LogsOutput{}, err
	}
	defer resp.Body.Close()
	output := LogsOutput{JobID: jobID, Source: source, TailLines: tailLines, MaxBytes: maxBytes}
	entries, returned, err := readBoundedLogs(resp.Body, source, tailLines, maxBytes, selectedLogs, declared)
	if err != nil {
		return LogsOutput{}, err
	}
	output.Entries = entries
	output.Returned = returned
	output.ArchiveDone = true
	return output, nil
}

func (c *Client) resultResponse(ctx context.Context, jobID string) (*http.Response, error) {
	return c.rawRequest(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(jobID)+"/result", nil, "")
}

func requireJobResult(job protocol.Job) error {
	if job.Result != nil && (job.Status == "succeeded" || job.Status == "failed") {
		return nil
	}
	return &ResultStateError{Status: job.Status}
}

func artifactID(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:16])
}

func validateArtifactMetadata(artifact protocol.Artifact) error {
	clean := filepath.Clean(filepath.FromSlash(artifact.Path))
	if artifact.Path == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != artifact.Path {
		return fmt.Errorf("job declares unsafe artifact path %q", artifact.Path)
	}
	if artifact.Size < 0 || len(artifact.SHA256) != 64 {
		return fmt.Errorf("job declares invalid metadata for artifact %q", artifact.Path)
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil {
		return fmt.Errorf("job declares invalid SHA-256 for artifact %q", artifact.Path)
	}
	return nil
}

func artifactMIMEType(path string) string {
	if strings.EqualFold(filepath.Ext(path), ".log") || strings.EqualFold(filepath.Ext(path), ".tex") {
		return "text/plain; charset=utf-8"
	}
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); value != "" {
		return value
	}
	return "application/octet-stream"
}

func extractSelectedArtifact(r io.Reader, outputRoot string, selected ArtifactInfo) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open result gzip: %w", err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	target := "artifacts/" + selected.Path
	entries := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read result tar: %w", err)
		}
		entries++
		if entries > 20_000 || header.Size < 0 || header.Size > 512<<20 {
			return errors.New("result archive exceeds safety limits")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("unexpected result entry type for %q", header.Name)
		}
		if header.Name != target {
			continue
		}
		if header.Size != selected.Size {
			return errors.New("artifact size does not match job metadata")
		}
		if err := writeArtifact(outputRoot, selected.Path, tarReader, selected.Size, selected.SHA256); err != nil {
			return err
		}
		return nil
	}
	return errors.New("result archive omitted the selected artifact")
}

func readBoundedLogs(r io.Reader, source string, tailLines int, maxBytes int64, selectedLogs int, declared map[string]protocol.Artifact) ([]LogEntry, int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("open result gzip: %w", err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	entries := make([]LogEntry, 0, 4)
	var perEntry int64
	var extra int64
	if selectedLogs > 0 {
		perEntry = maxBytes / int64(selectedLogs)
		extra = maxBytes % int64(selectedLogs)
	}
	var returnedTotal int64
	archiveEntries := 0
	selectedEntries := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read result tar: %w", err)
		}
		archiveEntries++
		if archiveEntries > 20_000 || header.Size < 0 || header.Size > 512<<20 {
			return nil, 0, errors.New("result archive exceeds safety limits")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, 0, fmt.Errorf("unexpected result entry type for %q", header.Name)
		}
		entrySource, path, expected, selected := classifyLogEntry(header.Name, source, declared)
		if !selected {
			continue
		}
		selectedEntries++
		if selectedEntries > 32 {
			return nil, 0, errors.New("result archive contains too many selected log files")
		}
		limit := perEntry
		if int64(selectedEntries) <= extra {
			limit++
		}
		buffer := newTailBuffer(int(limit))
		hash := sha256.New()
		writer := io.Writer(buffer)
		if expected != nil {
			writer = io.MultiWriter(hash, buffer)
		}
		if _, err := io.CopyN(writer, tarReader, header.Size); err != nil {
			return nil, 0, err
		}
		if expected != nil {
			if header.Size != expected.Size {
				return nil, 0, fmt.Errorf("compiler log %q size mismatch", path)
			}
			if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected.SHA256) {
				return nil, 0, fmt.Errorf("compiler log %q SHA-256 mismatch", path)
			}
		}
		content := tailLinesBytes(buffer.Bytes(), tailLines)
		content = []byte(strings.ToValidUTF8(string(content), "�"))
		returned := int64(len(content))
		returnedTotal += returned
		entries = append(entries, LogEntry{
			Source: entrySource, Path: path, Content: string(content), TotalBytes: header.Size,
			ReturnedBytes: returned, Truncated: returned < header.Size,
		})
	}
	return entries, returnedTotal, nil
}

func classifyLogEntry(name, source string, declared map[string]protocol.Artifact) (string, string, *protocol.Artifact, bool) {
	switch name {
	case "stdout.log":
		return "stdout", name, nil, source == "all" || source == "stdout"
	case "stderr.log":
		return "stderr", name, nil, source == "all" || source == "stderr"
	}
	if !strings.HasPrefix(name, "artifacts/") {
		return "", "", nil, false
	}
	path := strings.TrimPrefix(name, "artifacts/")
	artifact, ok := declared[path]
	if !ok || !strings.EqualFold(filepath.Ext(path), ".log") {
		return "", "", nil, false
	}
	return "compiler", path, &artifact, source == "all" || source == "compiler"
}

type tailBuffer struct {
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer {
	if limit < 0 {
		limit = 0
	}
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit == 0 {
		return n, nil
	}
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	overflow := len(b.data) + len(p) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *tailBuffer) Bytes() []byte {
	return b.data
}

func tailLinesBytes(data []byte, lines int) []byte {
	if lines < 1 || len(data) == 0 {
		return nil
	}
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == lines {
				return data[i+1:]
			}
		}
	}
	return data
}
