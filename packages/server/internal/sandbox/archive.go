// Package sandbox transports immutable inputs and disposable build checkpoints
// to resource-limited compiler containers. No host paths or credentials are mounted.
package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/config"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type archiveMember struct {
	name    string
	file    compile.File
	modTime time.Time
}

func writeArchive(w io.Writer, members []archiveMember, compression int) error {
	gz, err := gzip.NewWriterLevel(w, compression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, member := range members {
		f, err := member.file.Open()
		if err != nil {
			return err
		}
		err = tw.WriteHeader(
			&tar.Header{
				Name:     member.name,
				Mode:     0600,
				Size:     member.file.Size,
				ModTime:  member.modTime,
				Typeflag: tar.TypeReg,
			},
		)
		if err == nil {
			err = safefs.CopyVerified(tw, f, member.file.Size, member.file.SHA256)
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// collectState rejects links and nonregular files, including unused generated
// files that would otherwise hide outside the normal artifact allowlist.
func collectState(root string, maxFiles int, maxBytes int64) ([]archiveMember, error) {
	scoped, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()
	var members []archiveMember
	var total int64
	err = fs.WalkDir(scoped.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if len(members) >= maxFiles {
			return errors.New("checkpoint has too many files")
		}
		info, err := scoped.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("checkpoint contains a nonregular file")
		}
		file, err := describeFile(root, name, maxBytes-total)
		if err != nil {
			return err
		}
		total += file.Size
		members = append(members, archiveMember{name: name, file: file, modTime: info.ModTime()})
		return nil
	})
	return members, err
}

func describeFile(root, name string, maxBytes int64) (compile.File, error) {
	scoped, err := safefs.Open(root)
	if err != nil {
		return compile.File{}, err
	}
	defer func() { _ = scoped.Close() }()
	f, err := scoped.OpenRegular(name)
	if err != nil {
		return compile.File{}, err
	}
	defer func() { _ = f.Close() }()
	hash, size, err := safefs.Digest(f, maxBytes)
	return compile.File{Workspace: root, RelativePath: filepath.ToSlash(name), Size: size, SHA256: hash}, err
}

func writeArchiveFile(path string, members []archiveMember, maxBytes int64, compression int) error {
	scoped, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	return scoped.WriteExclusive(
		filepath.Base(path),
		maxBytes,
		func(w io.Writer) error { return writeArchive(w, members, compression) },
	)
}

func ensureDir(path string) error { return os.MkdirAll(path, 0700) }

// ArchiveCheckpoint uses the same bounded transport as full session state for
// previously verified portable auxiliaries restored by the project manager.
// Nonnegative limits bound raw files; compressed output gets the configured
// checkpoint overhead reserve. Invalid or overflowing limits are rejected.
func ArchiveCheckpoint(root, destination string, maxFiles int, maxBytes int64) error {
	if maxFiles < 0 || maxBytes < 0 || maxBytes > math.MaxInt64-config.CheckpointArchiveOverheadBytes-1 {
		return errors.New("invalid checkpoint limits")
	}
	members, err := collectState(root, maxFiles, maxBytes)
	if err != nil {
		return err
	}
	return writeArchiveFile(destination, members, maxBytes+config.CheckpointArchiveOverheadBytes, gzip.BestSpeed)
}
