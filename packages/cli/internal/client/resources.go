package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

type EnvironmentInventory struct {
	Baselines []resources.Baseline `json:"baselines"`
	Default   string               `json:"default"`
}

func (c *Client) Baselines(ctx context.Context) (EnvironmentInventory, error) {
	var result EnvironmentInventory
	err := c.jsonRequest(ctx, http.MethodGet, "/v1/environments", nil, &result)
	return result, err
}

func (c *Client) ResolveEnvironment(
	ctx context.Context,
	request resources.ResolveRequest,
) (resources.Environment, error) {
	var result resources.Environment
	normalized, err := resources.NormalizeRequest(request)
	if err != nil {
		return result, err
	}
	if err := c.jsonRequest(ctx, http.MethodPost, "/v1/environments/resolve", normalized, &result); err != nil {
		return result, err
	}
	return result, validateEnvironment(result)
}
func (c *Client) GetEnvironment(ctx context.Context, digest string) (resources.Environment, error) {
	var result resources.Environment
	if !resources.ValidDigest(digest) {
		return result, errors.New("invalid environment digest")
	}
	if err := c.jsonRequest(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(digest), nil, &result); err != nil {
		return result, err
	}
	if err := validateEnvironment(result); err != nil {
		return result, err
	}
	if result.Digest != digest {
		return result, errors.New("server returned a different environment")
	}
	return result, nil
}

func validateEnvironment(environment resources.Environment) error {
	digest, err := resources.EnvironmentDigest(environment)
	if err != nil {
		return err
	}
	if digest != environment.Digest {
		return errors.New("server environment digest does not match its manifest")
	}
	return nil
}

func (c *Client) PrepareEnvironment(
	ctx context.Context,
	environment resources.Environment,
	blobs map[string]projectarchive.File,
) (resources.Environment, error) {
	if err := validateEnvironment(environment); err != nil {
		return environment, err
	}
	pending, err := missingResourceBlobs(environment, blobs)
	if err != nil {
		return environment, err
	}
	for _, file := range pending {
		if err := c.uploadResourceBlob(ctx, environment.Digest, file); err != nil {
			return environment, err
		}
	}
	var prepared resources.Environment
	if err := c.jsonRequest(
		ctx,
		http.MethodPost,
		"/v1/environments/"+environment.Digest+"/prepare",
		nil,
		&prepared,
	); err != nil {
		return prepared, err
	}
	if err := validateEnvironment(prepared); err != nil {
		return prepared, err
	}
	if prepared.Digest != environment.Digest || !prepared.Ready {
		return prepared, errors.New("server did not prepare the requested environment")
	}
	return prepared, nil
}

// Validate every requested upload before sending any bytes. A local blob is
// eligible only when the resolved environment declares the same digest and size.
func missingResourceBlobs(
	environment resources.Environment,
	blobs map[string]projectarchive.File,
) ([]projectarchive.File, error) {
	expected := make(map[string]int64)
	for _, resource := range environment.Resources {
		for _, file := range resource.Files {
			if size, exists := expected[file.SHA256]; exists && size != file.Size {
				return nil, errors.New("environment declares inconsistent sizes for a resource blob")
			}
			expected[file.SHA256] = file.Size
		}
	}
	var pending []projectarchive.File
	seen := make(map[string]bool)
	for _, sha := range environment.Missing {
		size, declared := expected[sha]
		if !declared {
			return nil, fmt.Errorf("server requested undeclared resource blob %s", sha)
		}
		file, available := blobs[sha]
		if !available {
			return nil, fmt.Errorf("environment requires unavailable resource blob %s", sha)
		}
		if file.SHA256 != sha || file.Size != size {
			return nil, errors.New("resource blob metadata does not match the environment manifest")
		}
		if !seen[sha] {
			pending = append(pending, file)
			seen[sha] = true
		}
	}
	return pending, nil
}

func (c *Client) uploadResourceBlob(ctx context.Context, digest string, file projectarchive.File) (err error) {
	input, err := projectarchive.OpenFile(file)
	if err != nil {
		return err
	}
	// Keep one owner for the file descriptor. net/http closes its request-body
	// wrapper; closing the response then the file also covers early HTTP errors.
	defer func() { err = errors.Join(err, input.Close()) }()
	response, err := c.rawRequest(
		ctx,
		http.MethodPut,
		"/v1/environments/"+digest+"/blobs/"+file.SHA256,
		io.NopCloser(input),
		"application/octet-stream",
	)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func validateResourceCapability(request protocol.CompileRequest, meta protocol.Metadata) error {
	if request.EnvironmentDigest != "" &&
		(!meta.Capabilities.ResourceEnvironments || !meta.Capabilities.QueuedJobs || !meta.Capabilities.IncrementalUpload) {
		return &CapabilityError{Capability: "resource environments"}
	}
	return nil
}

// SetBaseline binds runtime policy checks and inventory-only font fallbacks to
// the selected environment. Project files with the same name retain precedence.
func (c *Client) SetBaseline(baseline resources.Baseline) {
	c.SelectedBaseline = &baseline
	byName := make(map[string]projectarchive.File)
	for _, font := range baseline.Fonts {
		name := path.Base(font.File)
		if name == "." || name == "/" || name == "" {
			continue
		}
		byName[name] = projectarchive.File{
			Path:     name,
			SHA256:   font.SHA256,
			External: true,
			Reason:   "baseline font " + baseline.ID,
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	c.BaselineFiles = nil
	for _, name := range names {
		c.BaselineFiles = append(c.BaselineFiles, byName[name])
	}
}
