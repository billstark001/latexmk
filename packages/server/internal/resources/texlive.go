package resources

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/platform/process"
	sharedresources "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const maxPackageMetadataBytes int64 = 100 << 20

type packageMetadata struct {
	Name        string
	Revision    int
	Checksum    string
	Depends     []string
	Relocatable bool
	hasBinary   bool
	unsafeFiles bool
}

var texlivePackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,127}$`)

// Retain package identity, dependencies and path safety while omitting catalogue
// descriptions and individual file inventories.
func parseTLPDB(reader io.Reader) (map[string]packageMetadata, error) {
	data, err := safefs.ReadLimited(reader, maxPackageMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("read TeX Live package database: %w", err)
	}
	packages := make(map[string]packageMetadata)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var current packageMetadata
	var revisionSeen, relocationSeen bool
	section := ""
	flush := func() error {
		if current.Name == "" {
			return nil
		}
		if _, duplicate := packages[current.Name]; duplicate {
			return fmt.Errorf("duplicate TeX Live package %q", current.Name)
		}
		slices.Sort(current.Depends)
		current.Depends = slices.Compact(current.Depends)
		current.Relocatable = current.Relocatable && !current.hasBinary && !current.unsafeFiles
		packages[current.Name] = current
		current = packageMetadata{}
		revisionSeen, relocationSeen = false, false
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if line[0] == '#' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if section == "runfiles" || section == "srcfiles" || section == "docfiles" || section == "binfiles" {
				name := strings.TrimSpace(line)
				if section == "docfiles" {
					name, _, _ = strings.Cut(name, " ")
				}
				name = strings.TrimPrefix(name, "RELOC/")
				if clean, err := safefs.Clean(name); err != nil || clean != name {
					current.unsafeFiles = true
				}
			}
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)
		if strings.HasPrefix(key, "location-url\t") || key == "location-url" {
			continue
		}
		section = key
		if key == "name" {
			if current.Name != "" {
				return nil, errors.New("TeX Live package records must be separated by a blank line")
			}
			if !texlivePackageName.MatchString(value) {
				return nil, fmt.Errorf("invalid TeX Live package name %q", value)
			}
			current.Name = value
			continue
		}
		if current.Name == "" {
			return nil, errors.New("TeX Live package database has content before its first package")
		}
		switch key {
		case "revision":
			revision, err := strconv.Atoi(value)
			if err != nil || revision < 0 || revisionSeen {
				return nil, fmt.Errorf("invalid revision for TeX Live package %q", current.Name)
			}
			revisionSeen = true
			current.Revision = revision
		case "containerchecksum":
			if !validArchiveChecksum(value) || current.Checksum != "" {
				return nil, fmt.Errorf("invalid archive checksum for TeX Live package %q", current.Name)
			}
			current.Checksum = value
		case "depend":
			if value != "" {
				current.Depends = append(current.Depends, value)
			}
		case "relocated":
			if relocationSeen || (value != "0" && value != "1") {
				return nil, fmt.Errorf("invalid relocation flag for TeX Live package %q", current.Name)
			}
			relocationSeen = true
			current.Relocatable = value == "1"
		case "binfiles":
			current.hasBinary = true
		case "latexmk-has-binary", "latexmk-unsafe-files":
			if value != "0" && value != "1" {
				return nil, fmt.Errorf("invalid cached package safety marker for %q", current.Name)
			}
			if key == "latexmk-has-binary" {
				current.hasBinary = current.hasBinary || value == "1"
			} else {
				current.unsafeFiles = current.unsafeFiles || value == "1"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(packages) == 0 {
		return nil, errors.New("TeX Live package database is empty")
	}
	return packages, nil
}

func validArchiveChecksum(checksum string) bool {
	if len(checksum) != sha512.Size*2 || checksum != strings.ToLower(checksum) {
		return false
	}
	_, err := hex.DecodeString(checksum)
	return err == nil
}

// Installed baseline packages already satisfy their dependencies. Every added
// package is resolved transitively against exactly one registered snapshot.
func resolvePackageClosure(
	baseline []sharedresources.Package,
	requested []string,
	remote map[string]packageMetadata,
) ([]sharedresources.Package, error) {
	installed := make(map[string]bool, len(baseline))
	for _, pkg := range baseline {
		installed[pkg.Name] = true
	}
	visited := make(map[string]bool)
	var result []sharedresources.Package
	var visit func(string) error
	visit = func(name string) error {
		if !texlivePackageName.MatchString(name) {
			return fmt.Errorf("unsupported TeX Live dependency %q", name)
		}
		if installed[name] || visited[name] {
			return nil
		}
		pkg, exists := remote[name]
		if !exists {
			return &sharedresources.AcquisitionError{
				Code:    "resource_package_unavailable",
				Stage:   "metadata-resolve",
				Reason:  "snapshot",
				Package: name,
			}
		}
		if !pkg.Relocatable || pkg.hasBinary || pkg.unsafeFiles || strings.HasPrefix(name, "collection-") ||
			strings.HasPrefix(name, "scheme-") {
			return &sharedresources.AcquisitionError{
				Code:     "resource_package_unsupported",
				Stage:    "metadata-resolve",
				Reason:   "runtime-required",
				Package:  name,
				Revision: pkg.Revision,
			}
		}
		if pkg.Revision <= 0 || !validArchiveChecksum(pkg.Checksum) {
			return &sharedresources.AcquisitionError{
				Code:     "resource_package_unsupported",
				Stage:    "metadata-resolve",
				Reason:   "archive-identity",
				Package:  name,
				Revision: pkg.Revision,
			}
		}
		visited[name] = true
		for _, dependency := range pkg.Depends {
			if err := visit(dependency); err != nil {
				return fmt.Errorf("dependency of %s: %w", name, err)
			}
		}
		result = append(result, sharedresources.Package{Name: name, Revision: pkg.Revision, Checksum: pkg.Checksum})
		return nil
	}
	for _, name := range requested {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(result, func(a, b sharedresources.Package) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

var packageMetadataSlot = make(chan struct{}, 1)

func resolvePackages(
	ctx context.Context,
	baseline sharedresources.Baseline,
	requested []string,
	metadataDir string,
	metadataLimit int64,
	index *sharedresources.PinnedPackageIndex,
) (packages []sharedresources.Package, err error) {
	sourceID := "baseline"
	defer func() { logIndexAcquisition(ctx, baseline, sourceID, err) }()
	if len(requested) == 0 {
		return nil, nil
	}
	installed := make(map[string]bool)
	for _, pkg := range baseline.Packages {
		installed[pkg.Name] = true
	}
	allInstalled := true
	for _, name := range requested {
		if !texlivePackageName.MatchString(name) || !installed[name] {
			allInstalled = false
			break
		}
	}
	if allInstalled {
		return nil, nil
	}
	if err := validatePackageRepository(baseline); err != nil {
		return nil, err
	}
	if index != nil {
		sourceID = "pinned-index"
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := index.Read(baseline, maxPackageMetadataBytes)
		if err != nil {
			return nil, err
		}
		remote, err := parseTLPDB(bytes.NewReader(data))
		if err != nil {
			return nil, sharedresources.AsAcquisitionError(
				err,
				"resource_index_invalid",
				"metadata-parse",
				"tlpdb",
				sharedresources.Package{},
			)
		}
		return resolvePackageClosure(baseline.Packages, requested, remote)
	}
	if metadataLimit <= 0 {
		return nil, errors.New("no cache capacity is available for the TeX Live package index")
	}
	metadataLimit = min(metadataLimit, maxPackageMetadataBytes)
	select {
	case packageMetadataSlot <- struct{}{}:
		defer func() { <-packageMetadataSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(metadataDir, 0700); err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(baseline.Digest + "\x00" + baseline.Repository))
	name := hex.EncodeToString(key[:]) + ".tlpdb"
	root, err := safefs.Open(metadataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, readErr := root.ReadLimited(name, metadataLimit)
	sourceID = "metadata-cache"
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if errors.Is(readErr, os.ErrNotExist) {
		sourceID = "snapshot"
		home, err := os.MkdirTemp(metadataDir, ".probe-")
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(home) }()
		if err := validateTeXLiveYear(ctx, baseline.TexliveYear, home, ""); err != nil {
			return nil, err
		}
		var output strings.Builder
		_, err = runBoundedPackageProcess(
			ctx,
			process.Spec{
				Name: "tlmgr",
				Args: []string{"--repository", baseline.Repository, "dump-tlpdb", "--remote"},
				Env: packageEnvironment(
					home,
					"",
				),
				Stdout:         &output,
				MaxStreamBytes: metadataLimit,
				MaxOutputBytes: 8192,
			},
			home,
			metadataLimit,
			20000,
		)
		if err != nil {
			return nil, sharedresources.AsAcquisitionError(
				err,
				"resource_upstream_network",
				"metadata-fetch",
				"tlmgr",
				sharedresources.Package{},
			)
		}
		remote, err := parseTLPDB(strings.NewReader(output.String()))
		if err != nil {
			return nil, sharedresources.AsAcquisitionError(
				err,
				"resource_upstream_format",
				"metadata-parse",
				"tlpdb",
				sharedresources.Package{},
			)
		}
		data = packageIndex(remote)
		if int64(len(data)) > metadataLimit {
			return nil, &sharedresources.AcquisitionError{
				Code:   "resource_limit_exceeded",
				Stage:  "metadata-parse",
				Reason: "bytes",
				Bytes:  int64(len(data)),
				Limit:  metadataLimit,
			}
		}
		if _, err := root.WriteAtomic(
			name,
			metadataLimit,
			func(w io.Writer) error { _, err := w.Write(data); return err },
		); err != nil {
			return nil, sharedresources.AsAcquisitionError(
				err,
				"resource_storage_failed",
				"metadata-cache",
				"write",
				sharedresources.Package{},
			)
		}
	}
	remote, err := parseTLPDB(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return resolvePackageClosure(baseline.Packages, requested, remote)
}

// The full distribution database contains long descriptions and every file
// path. Only immutable package identity, dependencies and safety flags need to
// occupy persistent service storage.
func packageIndex(packages map[string]packageMetadata) []byte {
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	slices.Sort(names)
	var index strings.Builder
	index.WriteString("# latexmk-package-index-v1\n")
	for _, name := range names {
		pkg := packages[name]
		fmt.Fprintf(&index, "name %s\nrevision %d\nrelocated %d\n", pkg.Name, pkg.Revision, boolDigit(pkg.Relocatable))
		if pkg.Checksum != "" {
			fmt.Fprintf(&index, "containerchecksum %s\n", pkg.Checksum)
		}
		for _, dependency := range pkg.Depends {
			fmt.Fprintf(&index, "depend %s\n", dependency)
		}
		if pkg.hasBinary {
			index.WriteString("latexmk-has-binary 1\n")
		}
		if pkg.unsafeFiles {
			index.WriteString("latexmk-unsafe-files 1\n")
		}
		index.WriteByte('\n')
	}
	return []byte(index.String())
}

func boolDigit(value bool) int {
	if value {
		return 1
	}
	return 0
}

func validatePackageRepository(baseline sharedresources.Baseline) error {
	u, err := url.Parse(baseline.Repository)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		baseline.TexliveYear < 2000 {
		return &sharedresources.AcquisitionError{
			Code:   "resource_runtime_unavailable",
			Stage:  "metadata-resolve",
			Reason: "snapshot",
		}
	}
	return nil
}

func packageEnvironment(home, tree string) []string {
	environment := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"HOME=" + home,
		"TMPDIR=" + home,
	}
	if tree != "" {
		environment = append(environment, "TEXMFHOME="+tree, "TEXMFVAR="+tree, "TEXMFCONFIG="+tree)
	}
	return environment
}

func validateTeXLiveYear(ctx context.Context, year int, home, tree string) error {
	run := process.Run(
		ctx,
		process.Spec{
			Name:           "tlmgr",
			Args:           []string{"--version"},
			Env:            packageEnvironment(home, tree),
			MaxOutputBytes: 8192,
		},
	)
	if run.Err != nil {
		return sharedresources.AsAcquisitionError(
			run.Err,
			"resource_runtime_unavailable",
			"runtime-probe",
			"tlmgr",
			sharedresources.Package{},
		)
	}
	if run.StdoutTruncated {
		return &sharedresources.AcquisitionError{
			Code:   "resource_runtime_unavailable",
			Stage:  "runtime-probe",
			Reason: "version-output-limit",
		}
	}
	if !regexp.MustCompile(`(?m)TeX Live[^\n]*\b` + strconv.Itoa(year) + `\b`).Match(run.Stdout) {
		return &sharedresources.AcquisitionError{
			Code:   "resource_runtime_unavailable",
			Stage:  "runtime-probe",
			Reason: "texlive-year",
		}
	}
	return nil
}

// preparePackages installs only SHA-512 verified, locked run archives. Installing
// local files prevents a changed repository from silently replacing locked bytes.
// The caller owns the staging tree and removes it if this function fails.
func preparePackages(
	ctx context.Context,
	baseline sharedresources.Baseline,
	pkgs []sharedresources.Package,
	archiveDir, tree string,
	maxBytes int64,
	maxFiles int,
	policy *sharedresources.SourcePolicy,
) error {
	if len(pkgs) == 0 {
		return nil
	}
	if err := validatePackageRepository(baseline); err != nil {
		return err
	}
	if maxBytes <= 0 || maxFiles <= 0 || maxBytes > (1<<62)-int64(maxFiles)*2048 {
		return errors.New("invalid package preparation limits")
	}
	if !filepath.IsAbs(tree) {
		return errors.New("package staging tree must be an absolute path")
	}
	if err := os.MkdirAll(tree, 0700); err != nil {
		return err
	}
	home, err := os.MkdirTemp(filepath.Dir(tree), ".tlmgr-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(home) }()
	if err := validateTeXLiveYear(ctx, baseline.TexliveYear, home, tree); err != nil {
		return err
	}
	var downloaded int64
	var expanded int64
	var entries int
	var archives []string
	client := sharedresources.NewArchiveHTTPClient()
	defer client.CloseIdleConnections()
	for _, pkg := range pkgs {
		if !texlivePackageName.MatchString(pkg.Name) || pkg.Revision <= 0 || !validArchiveChecksum(pkg.Checksum) {
			return errors.New("invalid locked TeX Live package")
		}
		archive, size, err := cachedPackageArchive(
			ctx,
			archiveDir,
			baseline.Repository,
			pkg,
			maxBytes-downloaded,
			client,
			policy,
		)
		if err != nil {
			return err
		}
		downloaded += size
		stats, err := validatePackageArchive(ctx, pkg, archive, home, maxBytes-expanded, maxFiles-entries)
		if err != nil {
			return err
		}
		expanded += stats.Bytes
		entries += stats.Files
		archives = append(archives, archive)
	}
	baseArgs := []string{"--usermode", "--usertree", tree}
	for _, args := range [][]string{{"init-usertree"}, {"option", "install_docfiles", "0"}, {"option", "install_srcfiles", "0"}, append([]string{"install", "--no-depends-at-all", "--file"}, archives...)} {
		if err := runPackagePreparation(
			ctx,
			home,
			tree,
			append(slices.Clone(baseArgs), args...),
			maxBytes,
			maxFiles,
		); err != nil {
			return err
		}
	}
	root, err := safefs.Open(tree)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	installedFile, err := root.OpenRegular("tlpkg/texlive.tlpdb")
	if err != nil {
		return err
	}
	installed, parseErr := parseTLPDB(installedFile)
	if err := errors.Join(parseErr, installedFile.Close()); err != nil {
		return err
	}
	for _, pkg := range pkgs {
		if actual, exists := installed[pkg.Name]; !exists || actual.Revision != pkg.Revision {
			return &sharedresources.AcquisitionError{
				Code:     "resource_install_failed",
				Stage:    "install-verify",
				Reason:   "revision",
				Package:  pkg.Name,
				Revision: pkg.Revision,
			}
		}
	}
	if err := regularizePackageTree(tree, maxBytes, maxFiles); err != nil {
		return err
	}
	return checkPackageTree(tree, maxBytes, maxFiles, false)
}

var packageArchiveMu sync.Mutex
var packageArchiveFlights = make(map[string]chan struct{})

func acquirePackageArchive(ctx context.Context, directory, checksum string) (func(), error) {
	key := directory + "\x00" + checksum
	for {
		packageArchiveMu.Lock()
		waiting, exists := packageArchiveFlights[key]
		if !exists {
			waiting = make(chan struct{})
			packageArchiveFlights[key] = waiting
			packageArchiveMu.Unlock()
			release := func() {
				packageArchiveMu.Lock()
				delete(packageArchiveFlights, key)
				close(waiting)
				packageArchiveMu.Unlock()
			}
			return release, nil
		}
		packageArchiveMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waiting:
		}
	}
}

// Archives and prepared trees have independent identities and lifecycles. A
// failed tree can retain verified archive bytes for another environment to use.
func cachedPackageArchive(
	ctx context.Context,
	directory, repository string,
	pkg sharedresources.Package,
	limit int64,
	client *http.Client,
	policy *sharedresources.SourcePolicy,
) (path string, size int64, err error) {
	cacheHit := false
	var result sharedresources.ArchiveFetchResult
	defer func() { logArchiveAcquisition(ctx, pkg, cacheHit, result, err) }()
	if !validArchiveChecksum(pkg.Checksum) || limit <= 0 {
		return "", 0, errors.New("invalid archive cache identity or limit")
	}
	release, err := acquirePackageArchive(ctx, directory, pkg.Checksum)
	if err != nil {
		return "", 0, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", 0, sharedresources.AsAcquisitionError(err, "resource_storage_failed", "archive-cache", "mkdir", pkg)
	}
	root, err := safefs.Open(directory)
	if err != nil {
		return "", 0, sharedresources.AsAcquisitionError(err, "resource_storage_failed", "archive-cache", "open", pkg)
	}
	defer func() { _ = root.Close() }()
	name := pkg.Checksum + ".tar.xz"
	if file, err := root.OpenRegular(name); err == nil {
		info, statErr := file.Stat()
		if statErr == nil && info.Size() > limit {
			_ = file.Close()
			return "", 0, &sharedresources.AcquisitionError{
				Code:     "resource_limit_exceeded",
				Stage:    "archive-cache",
				Reason:   "bytes",
				Package:  pkg.Name,
				Revision: pkg.Revision,
				Bytes:    info.Size(),
				Limit:    limit,
			}
		}
		hash := sha512.New()
		size, readErr := io.Copy(hash, io.LimitReader(safefs.WithContext(ctx, file), limit+1))
		closeErr := file.Close()
		if err := errors.Join(statErr, readErr, closeErr); err != nil {
			return "", 0, sharedresources.AsAcquisitionError(
				err,
				"resource_storage_failed",
				"archive-cache",
				"read",
				pkg,
			)
		}
		if size <= limit && hex.EncodeToString(hash.Sum(nil)) == pkg.Checksum {
			cacheHit, result.Bytes = true, size
			return filepath.Join(directory, name), size, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", 0, sharedresources.AsAcquisitionError(err, "resource_storage_failed", "archive-cache", "open", pkg)
	}
	result, err = sharedresources.FetchArchive(ctx, sharedresources.ArchiveFetchOptions{
		Client: client, Repository: repository, Package: pkg, Policy: policy, MaxBytes: limit,
	}, func(write func(io.Writer) error) error {
		pending, err := root.Stage(name, limit, write)
		if err != nil {
			return err
		}
		defer func() { _ = pending.Close() }()
		if err := pending.Chmod(0444); err != nil {
			return err
		}
		return pending.Commit()
	})
	if err != nil {
		return "", 0, err
	}
	return filepath.Join(directory, name), result.Bytes, nil
}

type packageArchiveStats struct {
	Bytes int64
	Files int
}

func validatePackageArchive(
	ctx context.Context,
	pkg sharedresources.Package,
	archive, work string,
	maxBytes int64,
	maxFiles int,
) (packageArchiveStats, error) {
	var stats packageArchiveStats
	if maxBytes <= 0 || maxFiles <= 0 {
		return stats, &sharedresources.AcquisitionError{
			Code:     "resource_limit_exceeded",
			Stage:    "archive-validate",
			Reason:   "expanded-bytes",
			Package:  pkg.Name,
			Revision: pkg.Revision,
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	input, output := io.Pipe()
	finished := make(chan error, 1)
	go func() {
		run := process.Run(
			ctx,
			process.Spec{
				Name:           "xz",
				Args:           []string{"--decompress", "--stdout", "--", archive},
				Env:            packageEnvironment(work, ""),
				Stdout:         output,
				MaxStreamBytes: maxBytes + int64(maxFiles)*2048 + 1<<20,
				MaxOutputBytes: 8192,
			},
		)
		_ = output.CloseWithError(run.Err)
		finished <- run.Err
	}()
	err := inspectPackageTarCount(pkg, tar.NewReader(input), maxBytes, maxFiles, &stats)
	if err == nil {
		// tar stops at its end marker; drain bounded padding and wait for xz so a
		// truncated/corrupt compressor stream cannot be accepted as a valid tar.
		_, err = io.Copy(io.Discard, input)
	}
	if err != nil {
		cancel()
		_ = input.CloseWithError(err)
	} else {
		_ = input.Close()
	}
	if err := errors.Join(err, <-finished); err != nil {
		code, reason := "resource_archive_invalid", "xz-tar"
		if errors.Is(err, exec.ErrNotFound) {
			code, reason = "resource_runtime_unavailable", "xz"
		}
		return stats, sharedresources.AsAcquisitionError(err, code, "archive-validate", reason, pkg)
	}
	return stats, nil
}

func inspectPackageTar(pkg sharedresources.Package, reader *tar.Reader, maxBytes int64, maxFiles int) error {
	return inspectPackageTarCount(pkg, reader, maxBytes, maxFiles, nil)
}

func inspectPackageTarCount(
	pkg sharedresources.Package,
	reader *tar.Reader,
	maxBytes int64,
	maxFiles int,
	stats *packageArchiveStats,
) error {
	var total int64
	files := 0
	defer func() {
		if stats != nil {
			stats.Bytes, stats.Files = total, files
		}
	}()
	metadataFound := false
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if (name == "" || name == ".") && header.Typeflag == tar.TypeDir {
			continue
		}
		if name == "" || name == "." || path.Clean(name) != name || path.IsAbs(name) ||
			strings.ContainsAny(name, "\\\x00") ||
			name == ".." ||
			strings.HasPrefix(name, "../") {
			return errors.New("TeX Live archive contains an unsafe path")
		}
		if seen[name] {
			return errors.New("TeX Live archive contains a duplicate path")
		}
		seen[name] = true
		files++
		if files > maxFiles {
			return &sharedresources.AcquisitionError{
				Code:     "resource_limit_exceeded",
				Stage:    "archive-validate",
				Reason:   "files",
				Package:  pkg.Name,
				Revision: pkg.Revision,
				Bytes:    int64(files),
				Limit:    int64(maxFiles),
			}
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return errors.New("TeX Live archive contains a link or nonregular file")
		}
		if header.Size < 0 || header.Size > maxBytes-total {
			return &sharedresources.AcquisitionError{
				Code:     "resource_limit_exceeded",
				Stage:    "archive-validate",
				Reason:   "expanded-bytes",
				Package:  pkg.Name,
				Revision: pkg.Revision,
				Bytes:    total + header.Size,
				Limit:    maxBytes,
			}
		}
		total += header.Size
		if strings.HasPrefix(name, "tlpkg/tlpobj/") && strings.HasSuffix(name, ".tlpobj") {
			if metadataFound || name != "tlpkg/tlpobj/"+pkg.Name+".tlpobj" || header.Size > 1<<20 {
				return &sharedresources.AcquisitionError{
					Code:     "resource_archive_identity",
					Stage:    "archive-validate",
					Reason:   "metadata",
					Package:  pkg.Name,
					Revision: pkg.Revision,
				}
			}
			metadataFound = true
			metadata, err := parseTLPDB(io.LimitReader(reader, 1<<20))
			if err != nil {
				return err
			}
			actual, exists := metadata[pkg.Name]
			if len(metadata) != 1 || !exists || actual.Revision != pkg.Revision || !actual.Relocatable {
				return &sharedresources.AcquisitionError{
					Code:     "resource_archive_identity",
					Stage:    "archive-validate",
					Reason:   "revision-layout",
					Package:  pkg.Name,
					Revision: pkg.Revision,
				}
			}
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return err
		}
	}
	if !metadataFound {
		return &sharedresources.AcquisitionError{
			Code:     "resource_archive_identity",
			Stage:    "archive-validate",
			Reason:   "metadata-missing",
			Package:  pkg.Name,
			Revision: pkg.Revision,
		}
	}
	return nil
}

// A watchdog bounds preparation output as it is created, in addition to the
// final strict walk. Temporary input archives have their own bounded budget.
func runPackagePreparation(ctx context.Context, home, tree string, args []string, maxBytes int64, maxFiles int) error {
	_, err := runBoundedPackageProcess(
		ctx,
		process.Spec{Name: "tlmgr", Args: args, Env: packageEnvironment(home, tree), Dir: home, MaxOutputBytes: 8192},
		tree,
		maxBytes,
		maxFiles,
	)
	if err != nil {
		code, reason := "resource_install_failed", "tlmgr"
		if errors.Is(err, exec.ErrNotFound) {
			code, reason = "resource_runtime_unavailable", "tlmgr"
		}
		return sharedresources.AsAcquisitionError(err, code, "install", reason, sharedresources.Package{})
	}
	return nil
}

func runBoundedPackageProcess(
	ctx context.Context,
	spec process.Spec,
	tree string,
	maxBytes int64,
	maxFiles int,
) (process.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	checked := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				checked <- nil
				return
			case <-ctx.Done():
				checked <- nil
				return
			case <-ticker.C:
				if err := checkPackageTree(
					tree,
					maxBytes,
					maxFiles,
					true,
				); err != nil &&
					!errors.Is(err, fs.ErrNotExist) {
					checked <- err
					cancel()
					return
				}
			}
		}
	}()
	run := process.Run(ctx, spec)
	close(done)
	if err := errors.Join(run.Err, <-checked); err != nil {
		return run, err
	}
	return run, checkPackageTree(tree, maxBytes, maxFiles, true)
}

func checkPackageTree(tree string, maxBytes int64, maxFiles int, allowLinks bool) error {
	var total int64
	files := 0
	return filepath.WalkDir(tree, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if name == tree {
			return nil
		}
		files++
		if files > maxFiles {
			return &sharedresources.AcquisitionError{
				Code:   "resource_limit_exceeded",
				Stage:  "install",
				Reason: "files",
				Bytes:  int64(files),
				Limit:  int64(maxFiles),
			}
		}
		if info.IsDir() || (allowLinks && info.Mode()&os.ModeSymlink != 0) {
			return nil
		}
		if !info.Mode().IsRegular() {
			return &sharedresources.AcquisitionError{
				Code:   "resource_install_failed",
				Stage:  "install",
				Reason: "nonregular-file",
			}
		}
		if info.Size() < 0 || info.Size() > maxBytes-total {
			return &sharedresources.AcquisitionError{
				Code:   "resource_limit_exceeded",
				Stage:  "install",
				Reason: "expanded-bytes",
				Bytes:  total + info.Size(),
				Limit:  maxBytes,
			}
		}
		total += info.Size()
		return nil
	})
}

// updmap can emit relative aliases. Resolve only regular files within this
// private staging tree and replace aliases with bytes for regular-only workers.
func regularizePackageTree(tree string, maxBytes int64, maxFiles int) error {
	canonicalTree, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return err
	}
	var links []string
	if err := filepath.WalkDir(tree, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type()&os.ModeSymlink != 0 {
			links = append(links, name)
		}
		return err
	}); err != nil {
		return err
	}
	for _, link := range links {
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(canonicalTree, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
			filepath.IsAbs(relative) {
			return errors.New("prepared TeX Live tree contains an escaping link")
		}
		root, err := safefs.Open(tree)
		if err != nil {
			return err
		}
		input, err := root.OpenRegular(filepath.ToSlash(relative))
		if err != nil {
			_ = root.Close()
			return err
		}
		data, readErr := safefs.ReadLimited(input, maxBytes)
		closeErr := errors.Join(input.Close(), root.Close())
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if err := os.Remove(link); err != nil {
			return err
		}
		if err := os.WriteFile(link, data, 0600); err != nil {
			return err
		}
		if err := checkPackageTree(tree, maxBytes, maxFiles, true); err != nil {
			return err
		}
	}
	return nil
}
