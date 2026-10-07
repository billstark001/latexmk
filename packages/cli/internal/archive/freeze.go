package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type Frozen struct {
	Files       []File
	ReusedBytes int64
	root        string
}

func (f *Frozen) Close() error { return os.RemoveAll(f.root) }

// Freeze captures policy-approved sources in a private spool. Previous large
// captures can be linked after hashing current content; changed files are copied
// and hashed together so an editor save cannot change uploaded snapshot bytes.
func Freeze(ctx context.Context, files []File, maxFiles int, maxBytes int64, previous *Frozen) (_ *Frozen, err error) {
	if len(files) > maxFiles || maxFiles <= 0 || maxBytes <= 0 {
		return nil, errors.New("snapshot exceeds file or byte limits")
	}
	dir, err := os.MkdirTemp("", "latexmk-snapshot-")
	if err != nil {
		return nil, err
	}
	frozen := &Frozen{root: dir}
	complete := false
	defer func() {
		if !complete {
			err = errors.Join(err, frozen.Close())
		}
	}()
	// Reuse only large captured files. Current sources are still opened through
	// policy and fully hashed; metadata alone never authorizes content reuse.
	reusable := make(map[string]File)
	if previous != nil {
		for _, file := range previous.Files {
			if file.Size >= 256<<10 {
				reusable[file.Path] = file
			}
		}
	}
	var total int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		in, err := OpenFile(file)
		if err != nil {
			return nil, err
		}
		destination := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			_ = in.Close()
			return nil, err
		}
		if cached, ok := reusable[file.Path]; ok {
			hash, size, digestErr := safefs.Digest(safefs.WithContext(ctx, in), maxBytes-total)
			if digestErr != nil {
				return nil, errors.Join(digestErr, in.Close())
			}
			cachedSource := filepath.Join(previous.root, filepath.FromSlash(file.Path))
			if hash == cached.SHA256 && size == cached.Size && os.Link(cachedSource, destination) == nil {
				if err := in.Close(); err != nil {
					return nil, err
				}
				file.Source, file.Size, file.SHA256 = destination, size, hash
				frozen.Files = append(frozen.Files, file)
				frozen.ReusedBytes += size
				total += size
				continue
			}
			// A changed source or unsupported hard links uses ordinary capture.
			if _, err := in.Seek(0, io.SeekStart); err != nil {
				return nil, errors.Join(err, in.Close())
			}
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = in.Close()
			return nil, err
		}
		hash, size, copyErr := safefs.Digest(io.TeeReader(safefs.WithContext(ctx, in), out), maxBytes-total)
		err = errors.Join(copyErr, in.Close(), out.Close())
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxBytes {
			return nil, fmt.Errorf("snapshot exceeds %d bytes", maxBytes)
		}
		file.Source, file.Size, file.SHA256 = destination, size, hash
		frozen.Files = append(frozen.Files, file)
	}
	complete = true
	return frozen, nil
}
