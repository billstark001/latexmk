package compile

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type ResourceFile struct {
	Path string
	File File
}

// ResourceInput contains verified descriptors, never environment variables or commands.
type ResourceInput struct {
	Files      []ResourceFile
	TexmfFiles []ResourceFile
	FontDirs   []string
	FontLookup string
}

// DescribeResourceTree rejects links and bounds the expanded tree before transport.
func DescribeResourceTree(root string, maxFiles int, maxBytes int64) ([]ResourceFile, error) {
	if root == "" {
		return nil, nil
	}
	scoped, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()
	var result []ResourceFile
	var total int64
	err = fs.WalkDir(scoped.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if len(result) >= maxFiles {
			return errors.New("resource tree has too many files")
		}
		file, err := scoped.OpenRegular(name)
		if err != nil {
			return err
		}
		digest, size, readErr := safefs.Digest(file, maxBytes-total)
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		total += size
		result = append(result, ResourceFile{Path: name, File: File{
			Workspace: root, RelativePath: name, Size: size, SHA256: digest,
		}})
		return nil
	})
	return result, err
}

// MaterializeResources copies bytes into a private attempt; compilers never access cache files.
func MaterializeResources(workspace string, input ResourceInput) error {
	scoped, err := safefs.Open(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	copyFile := func(name string, file File) error {
		if old, err := scoped.OpenRegular(name); err == nil {
			check := safefs.CopyVerified(io.Discard, old, file.Size, file.SHA256)
			if err := errors.Join(check, old.Close()); err != nil {
				return fmt.Errorf("resource conflicts with source %q: %w", name, err)
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := scoped.MakeDirs(path.Dir(name), 0700); err != nil {
			return err
		}
		if err := scoped.WriteExclusive(name, file.Size, func(output io.Writer) error {
			input, err := file.Open()
			if err != nil {
				return err
			}
			return errors.Join(safefs.CopyVerified(output, input, file.Size, file.SHA256), input.Close())
		}); err != nil {
			return err
		}
		return scoped.Chmod(name, 0444)
	}
	for _, file := range input.Files {
		name, err := resources.Destination(".", file.Path)
		if err != nil {
			return err
		}
		if err := copyFile(name, file.File); err != nil {
			return err
		}
	}
	for _, file := range input.TexmfFiles {
		name, err := safefs.Clean(file.Path)
		if err != nil || name != file.Path {
			return errors.New("invalid TEXMF resource path")
		}
		if err := copyFile(path.Join(".latexmk-home/texmf", name), file.File); err != nil {
			return err
		}
	}
	return nil
}

func resourceFontEnvironment(workspace string, input ResourceInput) ([]string, error) {
	if input.FontLookup != "names" || (len(input.FontDirs) == 0 && len(input.TexmfFiles) == 0) {
		return nil, nil
	}
	// Fontconfig and luaotfload keep separate databases; all writable state is private.
	type fontConfig struct {
		XMLName xml.Name `xml:"fontconfig"`
		Include string   `xml:"include"`
		Dirs    []string `xml:"dir"`
		Cache   string   `xml:"cachedir"`
	}
	config := fontConfig{
		Include: "/etc/fonts/fonts.conf",
		Cache:   filepath.Join(workspace, ".latexmk-home/fontconfig-cache"),
	}
	for _, dir := range input.FontDirs {
		name, err := resources.MountPath(dir)
		if err != nil {
			return nil, err
		}
		config.Dirs = append(config.Dirs, filepath.Join(workspace, filepath.FromSlash(name)))
	}
	for _, file := range input.TexmfFiles {
		if strings.HasPrefix(file.Path, "fonts/") {
			config.Dirs = append(config.Dirs, filepath.Join(workspace, ".latexmk-home/texmf/fonts"))
			break
		}
	}
	if len(config.Dirs) == 0 {
		return nil, nil
	}
	raw, err := xml.Marshal(config)
	if err != nil {
		return nil, err
	}
	scoped, err := safefs.Open(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()
	if _, err := scoped.WriteAtomic(".latexmk-home/fonts.conf", int64(len(raw)), func(w io.Writer) error {
		_, err := w.Write(raw)
		return err
	}); err != nil {
		return nil, err
	}
	return []string{
		"FONTCONFIG_FILE=" + filepath.Join(workspace, ".latexmk-home/fonts.conf"),
		"XDG_CACHE_HOME=" + filepath.Join(workspace, ".latexmk-home/cache"),
		"OSFONTDIR=" + strings.Join(config.Dirs, string(os.PathListSeparator)),
	}, nil
}
