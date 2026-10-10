// Package pack exports verified source snapshots as portable ZIP archives.
package pack

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const MaxFiles = 20_000
const MaxBytes int64 = 2 << 30

func ValidateMode(mode string) error {
	if mode != "default" && mode != "arxiv" {
		return errors.New("pack mode must be default or arxiv")
	}
	return nil
}

// Sources keeps selected figures, including PDFs, while omitting known build
// intermediates and the entry's final PDF from an arXiv submission.
func Sources(files []projectarchive.File, entry, mode string) ([]projectarchive.File, error) {
	if err := ValidateMode(mode); err != nil {
		return nil, err
	}
	var selected []projectarchive.File
	stem := strings.TrimSuffix(path.Base(entry), path.Ext(entry))
	for _, file := range files {
		if mode == "arxiv" {
			for _, component := range strings.Split(file.Path, "/") {
				if strings.HasPrefix(component, ".") {
					return nil, fmt.Errorf(
						"arxiv removes hidden files; selected dependency %q cannot be packaged",
						file.Path,
					)
				}
			}
			if file.Path == stem+".pdf" || intermediate(file.Path) {
				continue
			}
		}
		selected = append(selected, file)
	}
	return selected, nil
}

func intermediate(name string) bool {
	for _, suffix := range []string{".aux", ".log", ".blg", ".bcf", ".fdb_latexmk", ".fls", ".run.xml",
		".synctex.gz", ".toc", ".lot", ".lof", ".dvi", ".xdv", ".idx", ".ilg", ".glo", ".glg"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			return true
		}
	}
	return false
}

func SubmissionArtifact(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".bbl", ".ind", ".gls", ".nls":
		return true
	default:
		return false
	}
}

// Merge never lets a generated file silently replace a captured source.
func Merge(sources, generated []projectarchive.File) ([]projectarchive.File, error) {
	byPath := make(map[string]projectarchive.File)
	for _, files := range [][]projectarchive.File{sources, generated} {
		for _, file := range files {
			if previous, ok := byPath[file.Path]; ok &&
				(previous.Size != file.Size || previous.SHA256 != file.SHA256) {
				return nil, fmt.Errorf("generated file conflicts with captured source %q", file.Path)
			}
			byPath[file.Path] = file
		}
	}
	files := make([]projectarchive.File, 0, len(byPath))
	for _, file := range byPath {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// Write publishes atomically only after every member passes its captured hash.
// Sorted paths, fixed timestamps and modes make identical snapshots reproducible.
func Write(ctx context.Context, output string, files []projectarchive.File) error {
	if !strings.EqualFold(filepath.Ext(output), ".zip") {
		return errors.New("pack output must have a .zip extension")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	root, err := safefs.Open(filepath.Dir(output))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	_, err = root.WriteAtomic(filepath.Base(output), MaxBytes+(32<<20), func(dst io.Writer) error {
		return writeZIP(ctx, dst, files)
	})
	return err
}

func writeZIP(ctx context.Context, dst io.Writer, files []projectarchive.File) (err error) {
	if len(files) > MaxFiles {
		return errors.New("pack exceeds file limit")
	}
	files = append([]projectarchive.File(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	writer := zip.NewWriter(dst)
	defer func() { err = errors.Join(err, writer.Close()) }()
	seen := make(map[string]bool)
	var total int64
	for _, file := range files {
		clean, err := safefs.Clean(file.Path)
		if err != nil || clean != file.Path || seen[file.Path] {
			return fmt.Errorf("unsafe or duplicate pack member %q", file.Path)
		}
		seen[file.Path] = true
		if file.Size < 0 || file.Size > MaxBytes-total {
			return errors.New("pack exceeds expanded byte limit")
		}
		total += file.Size
		header := &zip.FileHeader{Name: file.Path, Method: zip.Deflate,
			Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(0644)
		member, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		input, err := projectarchive.OpenFile(file)
		if err != nil {
			return err
		}
		err = errors.Join(
			safefs.CopyVerified(member, safefs.WithContext(ctx, input), file.Size, file.SHA256),
			input.Close(),
		)
		if err != nil {
			return err
		}
	}
	return nil
}
