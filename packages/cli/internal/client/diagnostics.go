package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

const (
	maxDiagnostics         = 100
	maxDiagnosticLocations = 8
	maxDiagnosticLineBytes = 64 << 10
	maxDiagnosticTextBytes = 4 << 10
)

var (
	fileLineDiagnosticPattern = regexp.MustCompile(`^(.+?):([0-9]+):\s*(.+)$`)
	sourceLinePattern         = regexp.MustCompile(`^l\.([0-9]+)\s*(.*)$`)
	inputLinePattern          = regexp.MustCompile(`(?i)\bon input line ([0-9]+)\b`)
	texOpenFilePattern        = regexp.MustCompile(`\(([^()\s]+\.tex)\b`)
	warningPattern            = regexp.MustCompile(`^(?:LaTeX|LaTeX Font|Package\s+\S+|Class\s+\S+) Warning:\s*(.+)$`)
)

type LogLocation struct {
	Source    string `json:"source"`
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

type Diagnostic struct {
	Severity           string        `json:"severity"`
	File               string        `json:"file,omitempty"`
	FileInferred       bool          `json:"fileInferred,omitempty"`
	Line               int           `json:"line,omitempty"`
	Message            string        `json:"message"`
	Context            string        `json:"context,omitempty"`
	LogLocations       []LogLocation `json:"logLocations"`
	LocationsTruncated bool          `json:"locationsTruncated,omitempty"`
}

type DiagnosticLog struct {
	Source         string `json:"source"`
	Path           string `json:"path"`
	TotalBytes     int64  `json:"totalBytes"`
	TotalLines     int    `json:"totalLines"`
	OversizedLines int    `json:"oversizedLines,omitempty"`
}

type DiagnosticsOutput struct {
	JobID       string          `json:"jobId"`
	Count       int             `json:"count"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
	LogsScanned []DiagnosticLog `json:"logsScanned"`
	Incomplete  bool            `json:"incomplete"`
}

type rawDiagnostic struct {
	Severity string
	File     string
	Inferred bool
	Line     int
	Message  string
	Context  string
	Location LogLocation
}

// Diagnostics creates a bounded index over the complete raw logs. It does not
// replace or alter the raw log retrieval path.
func (c *Client) Diagnostics(ctx context.Context, jobID string) (DiagnosticsOutput, error) {
	job, err := c.GetJob(ctx, jobID)
	if err != nil {
		return DiagnosticsOutput{}, err
	}
	if err := requireJobResult(job); err != nil {
		return DiagnosticsOutput{}, err
	}
	declared := make(map[string]protocol.Artifact, len(job.Result.Artifacts))
	for _, artifact := range job.Result.Artifacts {
		if err := validateArtifactMetadata(artifact); err != nil {
			return DiagnosticsOutput{}, err
		}
		if _, duplicate := declared[artifact.Path]; duplicate {
			return DiagnosticsOutput{}, fmt.Errorf("job declares duplicate artifact %q", artifact.Path)
		}
		declared[artifact.Path] = artifact
	}
	resp, err := c.resultResponse(ctx, jobID)
	if err != nil {
		return DiagnosticsOutput{}, err
	}
	defer resp.Body.Close()
	diagnostics, logs, incomplete, err := readDiagnostics(resp.Body, declared)
	if err != nil {
		return DiagnosticsOutput{}, err
	}
	return DiagnosticsOutput{
		JobID: jobID, Count: len(diagnostics), Diagnostics: diagnostics,
		LogsScanned: logs, Incomplete: incomplete,
	}, nil
}

func readDiagnostics(r io.Reader, declared map[string]protocol.Artifact) ([]Diagnostic, []DiagnosticLog, bool, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open result gzip: %w", err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	raw := make([]rawDiagnostic, 0, 16)
	logs := make([]DiagnosticLog, 0, 4)
	incomplete := false
	archiveEntries := 0
	selectedEntries := 0
	selectedPaths := make(map[string]struct{}, 4)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, false, fmt.Errorf("read result tar: %w", err)
		}
		archiveEntries++
		if archiveEntries > 20_000 || header.Size < 0 || header.Size > 512<<20 {
			return nil, nil, false, errors.New("result archive exceeds safety limits")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, nil, false, fmt.Errorf("unexpected result entry type for %q", header.Name)
		}
		source, path, expected, selected := classifyLogEntry(header.Name, "all", declared)
		if !selected {
			continue
		}
		selectedEntries++
		if selectedEntries > 32 {
			return nil, nil, false, errors.New("result archive contains too many selected log files")
		}
		logKey := source + "\x00" + path
		if _, duplicate := selectedPaths[logKey]; duplicate {
			return nil, nil, false, fmt.Errorf("result archive contains duplicate log %q", path)
		}
		selectedPaths[logKey] = struct{}{}
		parser := newDiagnosticStream(source, path)
		hash := sha256.New()
		writer := io.Writer(parser)
		if expected != nil {
			writer = io.MultiWriter(hash, parser)
		}
		if _, err := io.CopyN(writer, tarReader, header.Size); err != nil {
			return nil, nil, false, err
		}
		parser.finish()
		if expected != nil {
			if header.Size != expected.Size {
				return nil, nil, false, fmt.Errorf("compiler log %q size mismatch", path)
			}
			if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected.SHA256) {
				return nil, nil, false, fmt.Errorf("compiler log %q SHA-256 mismatch", path)
			}
		}
		raw = append(raw, parser.diagnostics...)
		logs = append(logs, DiagnosticLog{
			Source: source, Path: path, TotalBytes: header.Size,
			TotalLines: parser.lineNumber, OversizedLines: parser.oversizedLines,
		})
		incomplete = incomplete || parser.incomplete
	}
	diagnostics, mergeIncomplete := mergeDiagnostics(raw)
	return diagnostics, logs, incomplete || mergeIncomplete, nil
}

type diagnosticStream struct {
	source         string
	path           string
	pendingLine    []byte
	lineTooLong    bool
	lineNumber     int
	oversizedLines int
	currentFile    string
	pending        int
	diagnostics    []rawDiagnostic
	incomplete     bool
}

func newDiagnosticStream(source, path string) *diagnosticStream {
	return &diagnosticStream{source: source, path: path, pending: -1}
}

func (p *diagnosticStream) Write(data []byte) (int, error) {
	written := len(data)
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			p.appendLine(data)
			break
		}
		p.appendLine(data[:newline])
		p.consumeLine()
		data = data[newline+1:]
	}
	return written, nil
}

func (p *diagnosticStream) appendLine(fragment []byte) {
	remaining := maxDiagnosticLineBytes - len(p.pendingLine)
	if remaining > 0 {
		if len(fragment) < remaining {
			remaining = len(fragment)
		}
		p.pendingLine = append(p.pendingLine, fragment[:remaining]...)
	}
	if len(fragment) > remaining {
		p.lineTooLong = true
	}
}

func (p *diagnosticStream) consumeLine() {
	p.lineNumber++
	line := strings.TrimSuffix(strings.ToValidUTF8(string(p.pendingLine), "�"), "\r")
	if p.lineTooLong {
		p.oversizedLines++
		p.incomplete = true
	}
	p.parseLine(line)
	p.pendingLine = p.pendingLine[:0]
	p.lineTooLong = false
}

func (p *diagnosticStream) finish() {
	if len(p.pendingLine) > 0 || p.lineTooLong {
		p.consumeLine()
	}
}

func (p *diagnosticStream) parseLine(line string) {
	trimmed := strings.TrimSpace(line)
	if match := texOpenFilePattern.FindStringSubmatch(line); match != nil {
		if file := diagnosticFile(match[1]); file != "" {
			p.currentFile = file
		}
	}
	if match := fileLineDiagnosticPattern.FindStringSubmatch(trimmed); match != nil {
		lineNumber, err := strconv.Atoi(match[2])
		if err == nil {
			message := boundedDiagnosticText(strings.TrimSpace(match[3]))
			severity := diagnosticSeverity(message)
			file := diagnosticFile(match[1])
			if file != "" {
				p.currentFile = file
			}
			added := p.add(rawDiagnostic{
				Severity: severity, File: file, Line: lineNumber,
				Message: message, Location: p.location(p.lineNumber),
			})
			if added && severity == "error" {
				p.pending = len(p.diagnostics) - 1
			}
			return
		}
	}
	if match := sourceLinePattern.FindStringSubmatch(trimmed); match != nil && p.pending >= 0 && p.pending < len(p.diagnostics) {
		lineNumber, err := strconv.Atoi(match[1])
		if err == nil {
			p.diagnostics[p.pending].Line = lineNumber
			if p.diagnostics[p.pending].File == "" {
				p.diagnostics[p.pending].File = p.currentFile
				p.diagnostics[p.pending].Inferred = p.currentFile != ""
			}
			p.diagnostics[p.pending].Context = boundedDiagnosticText(strings.TrimSpace(match[2]))
			p.diagnostics[p.pending].Location.EndLine = p.lineNumber
		}
		p.pending = -1
		return
	}
	if match := warningPattern.FindStringSubmatch(trimmed); match != nil {
		p.add(rawDiagnostic{
			Severity: "warning", File: p.currentFile, Inferred: p.currentFile != "", Line: diagnosticInputLine(trimmed),
			Message: boundedDiagnosticText(trimmed), Location: p.location(p.lineNumber),
		})
		return
	}
	if strings.HasPrefix(trimmed, "!") {
		message := boundedDiagnosticText(strings.TrimSpace(strings.TrimPrefix(trimmed, "!")))
		if message != "" {
			added := p.add(rawDiagnostic{
				Severity: "error", File: p.currentFile, Inferred: p.currentFile != "", Message: message,
				Location: p.location(p.lineNumber),
			})
			if added {
				p.pending = len(p.diagnostics) - 1
			}
		}
		return
	}
	if strings.HasPrefix(trimmed, "Overfull ") || strings.HasPrefix(trimmed, "Underfull ") {
		p.add(rawDiagnostic{
			Severity: "warning", File: p.currentFile, Inferred: p.currentFile != "", Line: diagnosticInputLine(trimmed),
			Message: boundedDiagnosticText(trimmed), Location: p.location(p.lineNumber),
		})
		return
	}
	if strings.Contains(trimmed, "Fatal error occurred") {
		p.add(rawDiagnostic{
			Severity: "error", File: p.currentFile, Inferred: p.currentFile != "", Message: boundedDiagnosticText(trimmed),
			Location: p.location(p.lineNumber),
		})
	}
}

func (p *diagnosticStream) add(diagnostic rawDiagnostic) bool {
	if len(p.diagnostics) >= maxDiagnostics {
		p.incomplete = true
		p.pending = -1
		return false
	}
	p.diagnostics = append(p.diagnostics, diagnostic)
	return true
}

func (p *diagnosticStream) location(line int) LogLocation {
	return LogLocation{Source: p.source, Path: p.path, StartLine: line, EndLine: line}
}

func mergeDiagnostics(raw []rawDiagnostic) ([]Diagnostic, bool) {
	result := make([]Diagnostic, 0, len(raw))
	indices := make(map[string]int, len(raw))
	incomplete := false
	for _, item := range raw {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", item.Severity, item.File, item.Line, item.Message)
		if index, ok := indices[key]; ok {
			diagnostic := &result[index]
			if !item.Inferred {
				diagnostic.FileInferred = false
			}
			if !hasLogLocation(diagnostic.LogLocations, item.Location) {
				if len(diagnostic.LogLocations) >= maxDiagnosticLocations {
					diagnostic.LocationsTruncated = true
					incomplete = true
				} else {
					diagnostic.LogLocations = append(diagnostic.LogLocations, item.Location)
				}
			}
			if diagnostic.Context == "" {
				diagnostic.Context = item.Context
			}
			continue
		}
		if len(result) >= maxDiagnostics {
			incomplete = true
			continue
		}
		indices[key] = len(result)
		result = append(result, Diagnostic{
			Severity: item.Severity, File: item.File, FileInferred: item.Inferred, Line: item.Line, Message: item.Message,
			Context: item.Context, LogLocations: []LogLocation{item.Location},
		})
	}
	return result, incomplete
}

func hasLogLocation(locations []LogLocation, candidate LogLocation) bool {
	for _, location := range locations {
		if location == candidate {
			return true
		}
	}
	return false
}

func diagnosticSeverity(message string) string {
	if strings.Contains(strings.ToLower(message), "warning") {
		return "warning"
	}
	return "error"
}

func diagnosticInputLine(message string) int {
	match := inputLinePattern.FindStringSubmatch(message)
	if match == nil {
		return 0
	}
	line, _ := strconv.Atoi(match[1])
	return line
}

func diagnosticFile(value string) string {
	value = strings.Trim(strings.TrimSpace(value), "`'\"")
	value = strings.ReplaceAll(value, "\\", "/")
	if marker := strings.LastIndex(value, "/project/"); marker >= 0 {
		value = value[marker+len("/project/"):]
	}
	value = strings.TrimPrefix(value, "./")
	clean := path.Clean(value)
	windowsAbsolute := len(clean) >= 3 && clean[1] == ':' && clean[2] == '/'
	if clean == "" || clean == "." || path.IsAbs(clean) || windowsAbsolute || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func boundedDiagnosticText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= maxDiagnosticTextBytes {
		return value
	}
	return strings.ToValidUTF8(value[:maxDiagnosticTextBytes], "�") + "…"
}
