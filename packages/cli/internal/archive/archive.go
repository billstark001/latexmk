package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Options struct {
	Root     string
	Exclude  []string
	MaxFiles int
	MaxBytes int64
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
}

func Create(dst io.Writer, opts Options) (Stats, error) {
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)
	files, stats, err := Manifest(opts)
	if err != nil {
		return stats, err
	}
	for _, file := range files {
		info, err := os.Stat(file.Source)
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return stats, err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return stats, err
		}
		hdr.Name = file.Path
		hdr.Mode = 0o644
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return stats, err
		}
		f, err := os.Open(file.Source)
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return stats, err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			_ = tw.Close()
			_ = gz.Close()
			return stats, copyErr
		}
		if closeErr != nil {
			_ = tw.Close()
			_ = gz.Close()
			return stats, closeErr
		}
	}
	if err := tw.Close(); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return stats, err
	}
	if err := gz.Close(); err != nil {
		return stats, err
	}
	return stats, nil
}

func Manifest(opts Options) ([]File, Stats, error) {
	stats := Stats{}
	patterns, err := loadPatterns(opts.Root, opts.Exclude)
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
		if excluded(rel, d.IsDir(), patterns) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not supported: %s", rel)
		}
		if d.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type: %s", rel)
		}
		stats.Files++
		stats.Bytes += info.Size()
		if opts.MaxFiles > 0 && stats.Files > opts.MaxFiles {
			return fmt.Errorf("project contains more than %d files", opts.MaxFiles)
		}
		if opts.MaxBytes > 0 && stats.Bytes > opts.MaxBytes {
			return fmt.Errorf("project is larger than %d bytes", opts.MaxBytes)
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, Source: path, SHA256: digest, Size: info.Size()})
		return nil
	})
	if walkErr != nil {
		return nil, stats, walkErr
	}
	return files, stats, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func loadPatterns(root string, defaults []string) ([]string, error) {
	patterns := append([]string{}, defaults...)
	f, err := os.Open(filepath.Join(root, ".latexmkignore"))
	if os.IsNotExist(err) {
		return patterns, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, filepath.ToSlash(strings.TrimPrefix(line, "./")))
	}
	return patterns, s.Err()
}

func excluded(rel string, isDir bool, patterns []string) bool {
	for _, raw := range patterns {
		p := filepath.ToSlash(strings.TrimSpace(raw))
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			continue
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if !strings.Contains(p, "/") {
			parts := strings.Split(rel, "/")
			for _, part := range parts {
				if ok, _ := path.Match(p, part); ok {
					return true
				}
			}
		}
	}
	return false
}
