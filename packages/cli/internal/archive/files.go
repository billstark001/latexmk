package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
