package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// CompileCaptured builds exactly these captured members, without rediscovery,
// missing-file retries, recorder-cache updates or legacy transport fallback.
func (c *Client) CompileCaptured(
	ctx context.Context,
	request protocol.CompileRequest,
	snapshot *projectarchive.Frozen,
	outputRoot string,
	meta protocol.Metadata,
) (CompileOutput, error) {
	if !meta.Capabilities.IncrementalUpload || !meta.Capabilities.QueuedJobs {
		return CompileOutput{}, &CapabilityError{Capability: "captured queued compilation"}
	}
	if err := validateResourceCapability(request, meta); err != nil {
		return CompileOutput{}, err
	}
	if err := validateAuxiliaryCapability(request, meta); err != nil {
		return CompileOutput{}, err
	}
	request.RecordInputs = meta.Capabilities.DependencyInputs
	request.DetectMissingFiles = false
	output, err := c.compileQueued(ctx, request, outputRoot, snapshot.Files)
	if err != nil {
		return output, err
	}
	if output.Result.Entry != request.Entry || output.Result.Engine != request.Engine {
		return CompileOutput{}, errors.New("captured build result does not match the requested entry and engine")
	}
	if request.EnvironmentDigest != "" && output.Result.EnvironmentDigest != request.EnvironmentDigest {
		return CompileOutput{}, errors.New("captured build result does not match the requested environment")
	}
	return output, nil
}

// ExportEnvironmentResource downloads only explicitly exportable, hash-verified
// registered resources. The result owns a private portable-path capture.
func (c *Client) ExportEnvironmentResource(
	ctx context.Context,
	environment resources.Environment,
	id string,
) (*projectarchive.Frozen, error) {
	if !resources.ValidDigest(environment.Digest) || !resources.ValidID(id) {
		return nil, errors.New("invalid resource export identity")
	}
	if err := validateEnvironment(environment); err != nil {
		return nil, err
	}
	var selected *resources.Resource
	for i := range environment.Resources {
		if environment.Resources[i].ID == id {
			selected = &environment.Resources[i]
		}
	}
	if selected == nil || selected.Type != "server" || selected.Redistribution != "allow" || len(selected.Files) == 0 {
		return nil, fmt.Errorf("resource %q is not available for authorized export", id)
	}
	const maxExportFiles = 20_000
	const maxExportBytes int64 = 2 << 30
	if len(selected.Files) > maxExportFiles {
		return nil, errors.New("resource export exceeds file limit")
	}
	files := make([]projectarchive.File, 0, len(selected.Files))
	seen := make(map[string]bool)
	var total int64
	for _, file := range selected.Files {
		name, err := resources.Destination(selected.Mount, file.Path)
		if err != nil || seen[name] || file.Size < 0 || file.Size > maxExportBytes-total ||
			!resources.ValidDigest(file.SHA256) {
			return nil, errors.New("invalid resource export manifest")
		}
		seen[name] = true
		total += file.Size
		files = append(files, projectarchive.File{Path: name, SHA256: file.SHA256, Size: file.Size,
			Reason: "exported registered resource: " + id})
	}
	// Validate the whole manifest before fetching bytes. A file cannot also be
	// an ancestor directory of another member, regardless of manifest order.
	for _, file := range files {
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return nil, errors.New("resource export file conflicts with a parent directory")
			}
		}
	}
	root, err := os.MkdirTemp("", "latexmk-resource-export-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	for i := range files {
		file := &files[i]
		destination := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return nil, err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		response, err := c.rawRequest(ctx, http.MethodGet,
			"/v1/environments/"+environment.Digest+"/resources/"+id+"/blobs/"+file.SHA256, nil, "")
		if err != nil {
			return nil, errors.Join(err, output.Close())
		}
		digest, size, copyErr := safefs.Digest(io.TeeReader(response.Body, output), file.Size)
		if err := errors.Join(copyErr, response.Body.Close(), output.Close()); err != nil {
			return nil, err
		}
		if digest != file.SHA256 || size != file.Size {
			return nil, fmt.Errorf("exported resource member %q failed integrity verification", file.Path)
		}
		file.Source = destination
	}
	return projectarchive.Freeze(ctx, files, maxExportFiles, maxExportBytes, nil)
}
