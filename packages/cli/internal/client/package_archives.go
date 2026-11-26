package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/billstark001/latexmk/packages/shared/resources"
)

func (c *Client) PackageArchives(
	ctx context.Context,
	environment resources.Environment,
) (manifest resources.PackageArchives, err error) {
	if err := validateEnvironment(environment); err != nil {
		return manifest, err
	}
	response, err := c.rawRequest(
		ctx,
		http.MethodGet,
		"/v1/environments/"+environment.Digest+"/package-archives",
		nil,
		"application/json",
	)
	if err != nil {
		return manifest, err
	}
	defer func() { _ = response.Body.Close() }()
	defer func() {
		if err != nil {
			err = c.responseError(response, "invalid_api_response", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return manifest, errors.New("package archive manifest expected HTTP 200")
	}
	if err := decodeAPIResponse(response.Body, 8<<20, &manifest, true); err != nil {
		return manifest, err
	}
	if manifest.EnvironmentDigest != environment.Digest || len(manifest.Archives) != len(environment.Packages) {
		return manifest, errors.New("package archive manifest does not match the requested environment")
	}
	if manifest.Sources != nil {
		if manifest.Sources.PackageIndex != nil {
			return manifest, errors.New("package archive transport policy contains local index configuration")
		}
		if err := manifest.Sources.Validate(); err != nil {
			return manifest, err
		}
	}
	expected := make(map[string]resources.Package, len(environment.Packages))
	for _, pkg := range environment.Packages {
		expected[pkg.Name] = pkg
	}
	seen := make(map[string]bool, len(manifest.Archives))
	for _, archive := range manifest.Archives {
		pkg, exists := expected[archive.Package.Name]
		if !exists || seen[pkg.Name] || pkg != archive.Package || resources.ValidateArchivePackage(pkg) != nil ||
			archive.MaxBytes < 1 || archive.MaxBytes > 1<<40 {
			return manifest, errors.New("server returned an invalid or undeclared package archive")
		}
		seen[pkg.Name] = true
	}
	return manifest, nil
}

// UploadPackageArchive sends exactly the verified cache file's measured size.
// The caller retains ownership and its cache lease until the response completes.
func (c *Client) UploadPackageArchive(
	ctx context.Context,
	digest string,
	pkg resources.Package,
	file *os.File,
	size int64,
) error {
	if !resources.ValidDigest(digest) || resources.ValidateArchivePackage(pkg) != nil || size <= 0 {
		return errors.New("invalid package archive upload")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.BaseURL+"/v1/environments/"+digest+"/package-archives/"+pkg.Checksum,
		io.NewSectionReader(file, 0, size))
	if err != nil {
		return err
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/octet-stream")
	c.decorate(request)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return c.failedRequest(request, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return c.readHTTPError(response)
	}
	if response.StatusCode != http.StatusNoContent {
		return c.responseError(response, "invalid_api_response", errors.New("archive upload expected HTTP 204"))
	}
	return nil
}
