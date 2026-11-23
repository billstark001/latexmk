// Package resources captures project resources separately from source snapshots.
package resources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"

	"github.com/bmatcuk/doublestar/v4"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	shared "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const MaxFiles = 20_000
const MaxBytes int64 = 2 << 30

type Captured struct {
	Resources      []shared.Resource
	Blobs          map[string]projectarchive.File
	MountFilesByID map[string][]projectarchive.File
	DenyPaths      []string
	directory      string
	mounted        map[string]projectarchive.File
}

func (c *Captured) Close() error {
	if c == nil || c.directory == "" {
		return nil
	}
	if err := os.RemoveAll(c.directory); err != nil {
		return err
	}
	c.directory = ""
	c.mounted = nil
	return nil
}

func (c *Captured) MountFiles() []projectarchive.File {
	var files []projectarchive.File
	if c != nil {
		for _, resource := range c.Resources {
			files = append(files, c.MountFilesByID[resource.ID]...)
		}
	}
	return files
}

// PrivatePaths reserves every declared local resource source, including unused
// resources, so uploadMode all cannot bypass a publication's resource policy.
func PrivatePaths(root string, specs map[string]shared.Spec) ([]string, error) {
	var paths []string
	for id, spec := range specs {
		if spec.Type != "files" {
			continue
		}
		clean, err := safefs.Clean(spec.Path)
		if err != nil || clean != spec.Path {
			return nil, fmt.Errorf("resource %q path must stay inside project root", id)
		}
		paths = append(paths, filepath.Join(root, filepath.FromSlash(clean)))
	}
	sort.Strings(paths)
	return paths, nil
}

// Capture streams selected bytes once from the confined project into one owned
// spool. Resource-relative content and logical mounts share those frozen bytes.
func Capture(
	ctx context.Context,
	root string,
	specs map[string]shared.Spec,
	ids []string,
	denyFiles ...string,
) (_ *Captured, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	captured := &Captured{
		Blobs:          make(map[string]projectarchive.File),
		MountFilesByID: make(map[string][]projectarchive.File),
		mounted:        make(map[string]projectarchive.File),
	}
	captured.DenyPaths, err = PrivatePaths(root, specs)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, captured.Close())
		}
	}()
	ordered := slices.Clone(ids)
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	var totalBytes int64
	var totalFiles int
	for _, id := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !shared.ValidID(id) {
			return nil, fmt.Errorf("invalid resource ID %q", id)
		}
		spec, ok := specs[id]
		if !ok {
			return nil, fmt.Errorf("unknown resource %q", id)
		}
		resource := shared.Resource{
			ID:             id,
			Type:           spec.Type,
			Kind:           spec.Kind,
			Mount:          spec.Mount,
			Packages:       slices.Clone(spec.Packages),
			Ref:            spec.Ref,
			LicenseFiles:   slices.Clone(spec.LicenseFiles),
			Redistribution: spec.Redistribution,
		}
		if spec.Type == "files" {
			sources, err := localFiles(root, spec, denyFiles)
			if err != nil {
				return nil, fmt.Errorf("resource %q: %w", id, err)
			}
			if len(sources) == 0 {
				return nil, fmt.Errorf("resource %q selects no files", id)
			}
			if len(sources) > MaxFiles-totalFiles {
				return nil, errors.New("resources exceed file limit")
			}
			files, err := captured.captureFiles(ctx, root, id, spec.Mount, sources, MaxBytes-totalBytes)
			if err != nil {
				return nil, fmt.Errorf("resource %q: %w", id, err)
			}
			for _, file := range files {
				totalFiles++
				totalBytes += file.Size
				resource.Files = append(
					resource.Files,
					shared.File{Path: file.Path, SHA256: file.SHA256, Size: file.Size},
				)
				captured.Blobs[file.SHA256] = file
			}
		}
		captured.Resources = append(captured.Resources, resource)
	}
	normalized, err := shared.NormalizeRequest(shared.ResolveRequest{Resources: captured.Resources})
	if err != nil {
		return nil, err
	}
	captured.Resources = normalized.Resources
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return captured, nil
}

type localSource struct {
	member   string
	original string
}

func localFiles(root string, spec shared.Spec, denyFiles []string) ([]localSource, error) {
	clean, err := safefs.Clean(spec.Path)
	if err != nil || clean != spec.Path {
		return nil, errors.New("path must be clean and project-relative")
	}
	patterns, err := resourcePatterns(spec.Include)
	if err != nil {
		return nil, err
	}
	excludes, err := resourcePatterns(spec.Exclude)
	if err != nil {
		return nil, err
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fs.Close() }()
	source := filepath.Join(root, filepath.FromSlash(clean))
	info, err := fs.Lstat(filepath.FromSlash(clean))
	if err != nil {
		return nil, err
	}
	var files []projectarchive.File
	if info.IsDir() {
		if _, err := fs.ReadDir(clean); err != nil {
			return nil, err
		}
		files, _, err = projectarchive.Manifest(
			projectarchive.Options{
				Root:        source,
				IgnoreFiles: []string{},
				Exclude:     spec.Exclude,
				DeferHash:   true,
				MaxFiles:    MaxFiles,
				MaxBytes:    MaxBytes,
			},
		)
		if err != nil {
			return nil, err
		}
	} else {
		files = []projectarchive.File{{Path: path.Base(clean), Source: source}}
	}
	var selected []localSource
	for _, file := range files {
		included := len(patterns) == 0 || matchesAny(patterns, file.Path) ||
			slices.Contains(spec.LicenseFiles, file.Path)
		if !included || matchesAny(excludes, file.Path) {
			continue
		}
		original, err := filepath.Rel(root, file.Source)
		if err != nil {
			return nil, err
		}
		policy, err := projectarchive.Explain(
			projectarchive.Options{Root: root, IgnoreFiles: []string{}, DenyFiles: denyFiles},
			original,
		)
		if err != nil {
			return nil, err
		}
		if !policy.Allowed {
			return nil, fmt.Errorf("resource file %q is denied: %s", filepath.ToSlash(original), policy.Reason)
		}
		selected = append(selected, localSource{member: file.Path, original: filepath.ToSlash(original)})
	}
	return selected, nil
}

func resourcePatterns(values []string) ([]string, error) {
	patterns := make([]string, 0, len(values))
	for _, value := range values {
		pattern, err := dependency.NormalizePattern(value)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

func matchesAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if doublestar.MatchUnvalidated(pattern, name) {
			return true
		}
	}
	return false
}

func (c *Captured) captureFiles(
	ctx context.Context,
	root, id, mount string,
	sources []localSource,
	maxBytes int64,
) ([]projectarchive.File, error) {
	project, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = project.Close() }()
	if c.directory == "" {
		c.directory, err = os.MkdirTemp("", "latexmk-resources-")
		if err != nil {
			return nil, err
		}
	}
	spool, err := safefs.Open(c.directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = spool.Close() }()
	var files []projectarchive.File
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		destination, err := shared.Destination(mount, source.member)
		if err != nil {
			return nil, err
		}
		file, err := captureFile(ctx, project, spool, id, source, maxBytes)
		if err != nil {
			return nil, err
		}
		maxBytes -= file.Size
		file.Source = filepath.Join(c.directory, "content", id, filepath.FromSlash(source.member))
		mounted, err := c.mount(file, destination)
		if err != nil {
			return nil, err
		}
		mounted.Reason = "resource " + id
		c.MountFilesByID[id] = append(c.MountFilesByID[id], mounted)
		files = append(files, file)
	}
	return files, nil
}

func captureFile(
	ctx context.Context,
	project, spool *safefs.Root,
	id string,
	source localSource,
	limit int64,
) (_ projectarchive.File, err error) {
	file := projectarchive.File{Path: source.member}
	input, err := project.OpenRegular(source.original)
	if err != nil {
		return file, err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	file.SourcePermissions, err = projectarchive.CapturePermissions(input)
	if err != nil {
		return file, err
	}
	destination := path.Join("content", id, source.member)
	if err := spool.MakeDirs(path.Dir(destination), 0700); err != nil {
		return file, err
	}
	err = spool.WriteExclusive(destination, limit, func(output io.Writer) error {
		var copyErr error
		file.SHA256, file.Size, copyErr = safefs.Digest(io.TeeReader(safefs.WithContext(ctx, input), output), limit)
		return copyErr
	})
	return file, err
}

func (c *Captured) mount(file projectarchive.File, destination string) (projectarchive.File, error) {
	if existing, ok := c.mounted[destination]; ok {
		if existing.SHA256 != file.SHA256 || existing.Size != file.Size {
			return file, fmt.Errorf("resource mount collision: %s", destination)
		}
		// Keep this resource's original permissions when identical bytes share a mount.
		file.Path, file.Source = destination, existing.Source
		return file, nil
	}
	target := filepath.Join(c.directory, "mounted", filepath.FromSlash(destination))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return file, err
	}
	if err := os.Link(file.Source, target); err != nil {
		return file, err
	}
	file.Path, file.Source = destination, target
	c.mounted[destination] = file
	return file, nil
}

// FilterSources removes only managed logical resource members. A source with
// different bytes at the same destination is a collision, never an override.
func FilterSources(files, mounted []projectarchive.File) ([]projectarchive.File, error) {
	byPath := make(map[string]projectarchive.File, len(mounted))
	for _, file := range mounted {
		byPath[file.Path] = file
	}
	result := make([]projectarchive.File, 0, len(files))
	for _, file := range files {
		resource, ok := byPath[file.Path]
		if !ok {
			result = append(result, file)
			continue
		}
		if file.SHA256 != "" && resource.SHA256 != "" &&
			(file.SHA256 != resource.SHA256 || file.Size != resource.Size) {
			return nil, fmt.Errorf("source collides with resource destination %q", file.Path)
		}
	}
	return result, nil
}

// ControlPaths watches original resource roots rather than private spool files.
func ControlPaths(root string, specs map[string]shared.Spec, ids []string) []string {
	var paths []string
	for _, id := range ids {
		if spec, ok := specs[id]; ok && spec.Type == "files" {
			paths = append(paths, filepath.Join(root, filepath.FromSlash(spec.Path)))
		}
	}
	return paths
}
