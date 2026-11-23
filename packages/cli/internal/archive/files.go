package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// OpenFile revalidates a selected member at use time. Root-relative opens keep
// concurrent path replacements from redirecting reads outside the project.
func OpenFile(file File) (*os.File, error) {
	name := filepath.FromSlash(file.Path)
	if !filepath.IsLocal(name) || filepath.Clean(name) == "." {
		return nil, fmt.Errorf("invalid selected path %q", file.Path)
	}
	root := file.Source
	for range strings.Split(filepath.Clean(name), string(filepath.Separator)) {
		root = filepath.Dir(root)
	}
	if filepath.Clean(filepath.Join(root, name)) != filepath.Clean(file.Source) {
		return nil, fmt.Errorf("selected source does not match path %q", file.Path)
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fs.Close() }()
	return fs.OpenRegular(file.Path)
}

// ReadFile revalidates a selected source and bounds the actual bytes read,
// independently of its previous manifest size. Zero permits an empty file.
func ReadFile(file File, limit int64) ([]byte, error) {
	f, err := OpenFile(file)
	if err != nil {
		return nil, err
	}
	data, err := safefs.ReadLimited(f, limit)
	err = errors.Join(err, f.Close())
	if errors.Is(err, safefs.ErrLimit) {
		return nil, fmt.Errorf("file exceeds %d bytes: %s", limit, file.Path)
	}
	return data, err
}

// CapturePermissions reads metadata from the same confined regular-file handle
// used for capture. File type and privileged mode bits never become permissions.
func CapturePermissions(input *os.File) (*os.FileMode, error) {
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("permission source must be a regular file")
	}
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	permissions := info.Mode().Perm()
	return &permissions, nil
}
