package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/shared/jsonutil"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const MaxArchiveSources = 8

// SourcePolicy selects transport locations, independently of runtime identity.
// A nil policy preserves the original date-snapshot archive naming convention.
type SourcePolicy struct {
	Version          int                 `json:"version"`
	ArchiveMirrors   []string            `json:"archiveMirrors"`
	PerSourceTimeout string              `json:"perSourceTimeout,omitempty"`
	PackageIndex     *PinnedPackageIndex `json:"packageIndex,omitempty"`
}

// PinnedPackageIndex is an administrator assertion about a full remote TLPDB,
// not the installed package inventory. Path is local and must not cross the API.
type PinnedPackageIndex struct {
	Version     int    `json:"version,omitempty"`
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Repository  string `json:"repository"`
	TexliveYear int    `json:"texliveYear"`
}

func validateSourceURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.ContainsAny(value, "\r\n\t ") {
		return policyError("sources must be HTTPS base URLs without credentials, query or fragment")
	}
	return nil
}

func (p SourcePolicy) Validate() error {
	if p.Version != 1 || len(p.ArchiveMirrors) == 0 || len(p.ArchiveMirrors) > MaxArchiveSources {
		return policyError("version must be 1 and one to eight mirrors are required")
	}
	seen := map[string]bool{}
	for _, source := range p.ArchiveMirrors {
		if err := validateSourceURL(source); err != nil {
			return err
		}
		key := strings.TrimRight(source, "/")
		if seen[key] {
			return policyError("duplicate mirror")
		}
		seen[key] = true
	}
	if _, err := p.sourceTimeout(); err != nil {
		return err
	}
	if p.PackageIndex != nil {
		return p.PackageIndex.Validate()
	}
	return nil
}

func (p SourcePolicy) sourceTimeout() (time.Duration, error) {
	if p.PerSourceTimeout == "" {
		return 30 * time.Second, nil
	}
	duration, err := time.ParseDuration(p.PerSourceTimeout)
	if err != nil || duration <= 0 || duration > 5*time.Minute {
		return 0, policyError("perSourceTimeout must be positive and at most five minutes")
	}
	return duration, nil
}

func (p PinnedPackageIndex) Validate() error {
	if p.Version != 1 || p.Path == "" || !ValidDigest(p.SHA256) || p.TexliveYear < 2000 {
		return policyError("package index requires a path, SHA256, release year and supported version")
	}
	return validateSourceURL(p.Repository)
}

func LoadSourcePolicy(path string) (SourcePolicy, error) {
	var policy SourcePolicy
	file, err := safefs.OpenRegularFile(path)
	if err != nil {
		return policy, err
	}
	defer func() { _ = file.Close() }()
	if err := jsonutil.DecodeStrict(file, 64<<10, &policy); err != nil {
		return policy, policyError("cannot decode source policy")
	}
	if err := policy.Validate(); err != nil {
		return policy, err
	}
	if policy.PackageIndex != nil && !filepath.IsAbs(policy.PackageIndex.Path) {
		policy.PackageIndex.Path = filepath.Join(filepath.Dir(path), policy.PackageIndex.Path)
	}
	return policy, nil
}

func LoadPinnedPackageIndex(path string) (PinnedPackageIndex, error) {
	var index PinnedPackageIndex
	file, err := safefs.OpenRegularFile(path)
	if err != nil {
		return index, err
	}
	defer func() { _ = file.Close() }()
	if err := jsonutil.DecodeStrict(file, 64<<10, &index); err != nil || index.Version != 1 {
		return index, policyError("cannot decode version 1 package index assertion")
	}
	if err := index.Validate(); err != nil {
		return index, err
	}
	if !filepath.IsAbs(index.Path) {
		index.Path = filepath.Join(filepath.Dir(path), index.Path)
	}
	return index, nil
}

// Read verifies the asserted bytes and their snapshot binding on every use.
func (p PinnedPackageIndex) Read(baseline Baseline, limit int64) ([]byte, error) {
	failure := acquisitionFailure("resource_index_invalid", "metadata-read", "binding", Package{}, nil)
	if err := p.Validate(); err != nil || p.Repository != baseline.Repository || p.TexliveYear != baseline.TexliveYear {
		return nil, failure
	}
	file, err := safefs.OpenRegularFile(p.Path)
	if err != nil {
		return nil, AsAcquisitionError(err, "resource_storage_failed", "metadata-read", "read", Package{})
	}
	defer func() { _ = file.Close() }()
	if limit <= 0 || limit == math.MaxInt64 {
		return nil, acquisitionFailure("resource_limit_exceeded", "metadata-read", "bytes", Package{}, nil)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, AsAcquisitionError(err, "resource_storage_failed", "metadata-read", "read", Package{})
	}
	if int64(len(data)) > limit {
		return nil, &AcquisitionError{
			Code:   "resource_limit_exceeded",
			Stage:  "metadata-read",
			Reason: "bytes",
			Bytes:  int64(len(data)),
			Limit:  limit,
		}
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != p.SHA256 {
		failure.Reason, failure.Expected, failure.Actual = "checksum", p.SHA256, hex.EncodeToString(actual[:])
		return nil, failure
	}
	if len(data) == 0 {
		return nil, acquisitionFailure(
			"resource_index_invalid",
			"metadata-read",
			"empty",
			Package{},
			errors.New("empty index"),
		)
	}
	return data, nil
}
