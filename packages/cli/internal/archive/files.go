package archive

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	fs, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fs.Close() }()
	current := ""
	for _, part := range strings.Split(filepath.Clean(name), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := fs.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlinks are not supported: %s", file.Path)
		}
		if current == filepath.Clean(name) && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("selected file is not regular: %s", file.Path)
		}
	}
	f, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("selected file is not regular: %s", file.Path)
	}
	return f, nil
}

func ReadFile(file File, limit int64) ([]byte, error) {
	f, err := OpenFile(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes: %s", limit, file.Path)
	}
	return data, err
}
