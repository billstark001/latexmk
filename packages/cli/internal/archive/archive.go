// Package archive selects and packages local project files for upload.
package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type Options struct {
	IgnoreFiles      []string
	DenyFiles        []string
	DeferHash        bool
	Root             string
	Exclude          []string
	RespectGitIgnore bool
	MaxFiles         int
	MaxBytes         int64
}

type Stats struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// File is a validated, content-addressed project member. Manifest exposes the
// same selection rules as Create so incremental uploads cannot accidentally
// include a file that the legacy archive path would exclude.
type File struct {
	Path   string `json:"path"`
	Source string `json:"-"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Reason string `json:"reason,omitempty"`
}

// Create selects, hashes and archives project files under the configured policy.
// Source changes during creation cause failure; dst may contain partial output.
func Create(dst io.Writer, opts Options) (Stats, error) {
	opts.DeferHash = false
	files, stats, err := Manifest(opts)
	if err != nil {
		return stats, err
	}
	if err := CreateFiles(dst, files); err != nil {
		return stats, err
	}
	return stats, nil
}

// CreateFiles writes an exact content-addressed manifest, verifying both byte
// count and SHA-256 while copying. Callers needing immutable bytes across editor
// saves must freeze the manifest first. On error dst may contain partial output.
func CreateFiles(dst io.Writer, files []File) (err error) {
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, tw.Close(), gz.Close()) }()
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		clean, err := safefs.Clean(file.Path)
		if err != nil || clean != file.Path || seen[file.Path] {
			return fmt.Errorf("invalid or duplicate selected path %q", file.Path)
		}
		seen[file.Path] = true
		if err := writeSelectedMember(tw, file); err != nil {
			return fmt.Errorf("archive %s: %w", file.Path, err)
		}
	}
	return nil
}

func writeSelectedMember(tw *tar.Writer, file File) (err error) {
	f, err := OpenFile(file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if file.Size < 0 || file.Size != info.Size() {
		return errors.New("selected file size changed")
	}
	if digest, err := hex.DecodeString(file.SHA256); err != nil || len(digest) != sha256.Size {
		return errors.New("selected file requires a valid SHA-256 digest")
	}
	header := &tar.Header{Name: file.Path, Mode: 0644, Size: file.Size, ModTime: info.ModTime(), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	return safefs.CopyVerified(tw, f, file.Size, strings.ToLower(file.SHA256))
}

// Manifest applies upload policy and reports regular files in sorted path order.
// Zero limits are unbounded; negative limits are invalid. DeferHash returns stat
// metadata for selection, and requires later hashing/capture before publication.
func Manifest(opts Options) ([]File, Stats, error) {
	stats := Stats{}
	if opts.MaxFiles < 0 || opts.MaxBytes < 0 {
		return nil, stats, errors.New("invalid project limits")
	}
	policy, err := newPolicy(opts)
	if err != nil {
		return nil, stats, err
	}
	selection, err := loadGitSelection(opts.Root, opts.RespectGitIgnore)
	if err != nil {
		return nil, stats, err
	}
	files := make([]File, 0)
	walkErr := filepath.WalkDir(opts.Root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(opts.Root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		denied, _, policyErr := policy.excluded(rel, d.IsDir())
		if policyErr != nil {
			return policyErr
		}
		if denied {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if selection.Enabled {
				if _, ok := selection.Directories[rel]; !ok {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if selection.Enabled {
			if _, ok := selection.Files[rel]; !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not supported: %s", rel)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type: %s", rel)
		}
		stats.Files++
		if info.Size() < 0 || info.Size() > math.MaxInt64-stats.Bytes {
			return errors.New("project byte count overflows")
		}
		stats.Bytes += info.Size()
		if opts.MaxFiles > 0 && stats.Files > opts.MaxFiles {
			return fmt.Errorf("project contains more than %d files", opts.MaxFiles)
		}
		if !opts.DeferHash && opts.MaxBytes > 0 && stats.Bytes > opts.MaxBytes {
			return fmt.Errorf("project is larger than %d bytes", opts.MaxBytes)
		}
		digest := ""
		size := info.Size()
		if !opts.DeferHash {
			limit := opts.MaxBytes
			if limit == 0 || limit == math.MaxInt64 {
				limit = math.MaxInt64 - 1
			}
			prior := stats.Bytes - info.Size()
			digest, size, err = digestSelected(File{Path: rel, Source: path}, limit-prior)
			if err != nil {
				return err
			}
		}
		if !opts.DeferHash {
			stats.Bytes = stats.Bytes - info.Size() + size
		}
		files = append(files, File{Path: rel, Source: path, SHA256: digest, Size: size})
		return nil
	})
	if walkErr != nil {
		return nil, stats, walkErr
	}
	return files, stats, nil
}

type gitSelection struct {
	Enabled     bool
	Files       map[string]struct{}
	Directories map[string]struct{}
}

func loadGitSelection(root string, enabled bool) (gitSelection, error) {
	selection := gitSelection{}
	if !enabled || !hasGitMarker(root) {
		return selection, nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return selection, fmt.Errorf("resolve project root path: %w", err)
	}
	repoOutput, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return selection, fmt.Errorf("resolve Git root for %s: %w", root, err)
	}
	// Git terminates the path with a newline; other whitespace belongs to it.
	repoRoot := strings.TrimSuffix(string(repoOutput), "\n")
	repoRoot, err = filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return selection, fmt.Errorf("resolve Git root path: %w", err)
	}
	output, err := exec.Command(
		"git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--full-name", "--", ".",
	).Output()
	if err != nil {
		return selection, fmt.Errorf("list Git project files: %w", err)
	}
	selection.Enabled = true
	selection.Files = make(map[string]struct{})
	selection.Directories = make(map[string]struct{})
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		absolute := filepath.Join(repoRoot, filepath.FromSlash(string(raw)))
		rel, err := filepath.Rel(resolvedRoot, absolute)
		if err != nil {
			return gitSelection{}, err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		rel = filepath.ToSlash(rel)
		selection.Files[rel] = struct{}{}
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			selection.Directories[dir] = struct{}{}
		}
	}
	return selection, nil
}

func hasGitMarker(root string) bool {
	dir, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// HashSelected hashes the actual selected byte streams and updates size/digest
// together. Zero is unbounded; negative limits are invalid. On failure, earlier
// members may already have been updated, so callers must discard partial state.
func HashSelected(files []File, maxBytes int64) error {
	if maxBytes < 0 {
		return errors.New("invalid selected-file byte limit")
	}
	remaining := maxBytes
	if remaining == 0 || remaining == math.MaxInt64 {
		remaining = math.MaxInt64 - 1
	}
	for i := range files {
		digest, size, err := digestSelected(files[i], remaining)
		if err != nil {
			return fmt.Errorf("hash %s: %w", files[i].Path, err)
		}
		files[i].Size, files[i].SHA256 = size, digest
		remaining -= size
	}
	return nil
}

func digestSelected(file File, limit int64) (string, int64, error) {
	f, err := OpenFile(file)
	if err != nil {
		return "", 0, err
	}
	digest, size, err := safefs.Digest(f, limit)
	return digest, size, errors.Join(err, f.Close())
}
