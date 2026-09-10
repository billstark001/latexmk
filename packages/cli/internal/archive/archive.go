// Package archive selects and packages local project files for upload.
package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
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

func Create(dst io.Writer, opts Options) (Stats, error) {
	files, stats, err := Manifest(opts)
	if err != nil {
		return stats, err
	}
	if err := CreateFiles(dst, files); err != nil {
		return stats, err
	}
	return stats, nil
}

// CreateFiles writes a previously validated manifest. This keeps preview,
// incremental upload, and legacy archive upload on the same selected file set.
func CreateFiles(dst io.Writer, files []File) error {
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		f, err := OpenFile(file)
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			_ = tw.Close()
			_ = gz.Close()
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			_ = f.Close()
			_ = tw.Close()
			_ = gz.Close()
			return err
		}
		hdr.Name = file.Path
		hdr.Mode = 0o644
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			_ = tw.Close()
			_ = gz.Close()
			return err
		}
		_, copyErr := io.CopyN(tw, f, info.Size())
		closeErr := f.Close()
		if copyErr != nil {
			_ = tw.Close()
			_ = gz.Close()
			return copyErr
		}
		if closeErr != nil {
			_ = tw.Close()
			_ = gz.Close()
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return nil
}

func Manifest(opts Options) ([]File, Stats, error) {
	stats := Stats{}
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
		stats.Bytes += info.Size()
		if opts.MaxFiles > 0 && stats.Files > opts.MaxFiles {
			return fmt.Errorf("project contains more than %d files", opts.MaxFiles)
		}
		if !opts.DeferHash && opts.MaxBytes > 0 && stats.Bytes > opts.MaxBytes {
			return fmt.Errorf("project is larger than %d bytes", opts.MaxBytes)
		}
		digest := ""
		if !opts.DeferHash {
			digest, err = fileSHA256(File{Path: rel, Source: path})
			if err != nil {
				return err
			}
		}
		files = append(files, File{Path: rel, Source: path, SHA256: digest, Size: info.Size()})
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
	repoRoot := strings.TrimSpace(string(repoOutput))
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

func fileSHA256(file File) (string, error) {
	f, err := OpenFile(file)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, f, info.Size()); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// HashSelected finalizes only selected members, applying upload size limits afterwards.
func HashSelected(files []File, maxBytes int64) error {
	var total int64
	for i := range files {
		f, err := OpenFile(files[i])
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		files[i].Size = info.Size()
		total += info.Size()
		if maxBytes > 0 && total > maxBytes {
			_ = f.Close()
			return fmt.Errorf("selected files exceed %d bytes", maxBytes)
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(hash, f, info.Size())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		files[i].SHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	return nil
}
