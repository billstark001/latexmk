// Package archive extracts uploaded projects with path and resource limits.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Limits struct {
	MaxFiles int
	MaxBytes int64
}

type Stats struct {
	Files int
	Bytes int64
}

func ExtractTarGz(r io.Reader, root string, limits Limits) (Stats, error) {
	var stats Stats
	gz, err := gzip.NewReader(r)
	if err != nil {
		return stats, fmt.Errorf("invalid gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	seen := make(map[string]struct{})
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("invalid tar stream: %w", err)
		}
		clean, err := safePath(h.Name)
		if err != nil {
			return stats, err
		}
		if _, ok := seen[clean]; ok {
			return stats, fmt.Errorf("duplicate archive path %q", h.Name)
		}
		seen[clean] = struct{}{}
		dst := filepath.Join(root, filepath.FromSlash(clean))
		stats.Files++
		if limits.MaxFiles > 0 && stats.Files > limits.MaxFiles {
			return stats, fmt.Errorf("archive exceeds maximum entry count %d", limits.MaxFiles)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return stats, err
			}
		case tar.TypeReg:
			if h.Size < 0 {
				return stats, fmt.Errorf("negative size for %q", h.Name)
			}
			stats.Bytes += h.Size
			if limits.MaxBytes > 0 && stats.Bytes > limits.MaxBytes {
				return stats, fmt.Errorf("archive expands beyond %d bytes", limits.MaxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return stats, err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return stats, err
			}
			n, copyErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return stats, copyErr
			}
			if n != h.Size {
				return stats, io.ErrUnexpectedEOF
			}
			if closeErr != nil {
				return stats, closeErr
			}
			mtime := h.ModTime
			if mtime.IsZero() || mtime.After(time.Now().Add(5*time.Minute)) {
				mtime = time.Now()
			}
			_ = os.Chtimes(dst, mtime, mtime)
		default:
			return stats, fmt.Errorf("unsupported archive entry type for %q", h.Name)
		}
	}
	return stats, nil
}

func safePath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	name = strings.TrimPrefix(name, "./")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	if len(clean) > 4096 {
		return "", fmt.Errorf("archive path is too long")
	}
	return clean, nil
}
