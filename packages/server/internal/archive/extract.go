// Package archive extracts uploaded projects with path and resource limits.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/billstark001/latexmk/packages/shared/archiveutil"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type Limits struct {
	MaxFiles int
	MaxBytes int64
}

type Stats struct {
	Files int
	Bytes int64
}

// ExtractTarGz extracts only regular files and directories under an existing
// root, rejecting duplicate paths, links and corrupt gzip envelopes. Positive
// limits bound entry count (including directories) and regular-file bytes;
// zero limits are unbounded and negative limits are invalid. On failure, already
// extracted files remain in root.
func ExtractTarGz(r io.Reader, root string, limits Limits) (Stats, error) {
	var stats Stats
	if limits.MaxFiles < 0 || limits.MaxBytes < 0 {
		return stats, errors.New("invalid archive limits")
	}
	byteLimit := limits.MaxBytes
	if byteLimit == 0 {
		byteLimit = math.MaxInt64
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return stats, err
	}
	defer func() { _ = fs.Close() }()
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
		clean, err := safefs.Clean(h.Name)
		if err != nil {
			return stats, err
		}
		if _, ok := seen[clean]; ok {
			return stats, fmt.Errorf("duplicate archive path %q", h.Name)
		}
		seen[clean] = struct{}{}
		stats.Files++
		if limits.MaxFiles > 0 && stats.Files > limits.MaxFiles {
			return stats, fmt.Errorf("archive exceeds maximum entry count %d", limits.MaxFiles)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := fs.MakeDirs(clean, 0o755); err != nil {
				return stats, err
			}
		case tar.TypeReg:
			if h.Size < 0 {
				return stats, fmt.Errorf("negative size for %q", h.Name)
			}
			if h.Size > byteLimit-stats.Bytes {
				return stats, fmt.Errorf("archive expands beyond %d bytes", limits.MaxBytes)
			}
			stats.Bytes += h.Size
			if err := fs.WriteExclusive(clean, h.Size, func(w io.Writer) error {
				_, err := io.CopyN(w, tr, h.Size)
				return err
			}); err != nil {
				return stats, err
			}

			mtime := h.ModTime
			if mtime.IsZero() || mtime.After(time.Now().Add(5*time.Minute)) {
				mtime = time.Now()
			}
			_ = fs.Chtimes(clean, mtime, mtime)
		default:
			return stats, fmt.Errorf("unsupported archive entry type for %q", h.Name)
		}
	}
	return stats, archiveutil.VerifyTrailer(gz)
}
