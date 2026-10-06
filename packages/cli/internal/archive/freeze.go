package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Frozen struct {
	Files []File
	root  string
}

func (f *Frozen) Close() error { return os.RemoveAll(f.root) }

// Freeze reads each policy-approved source once into a private spool. Its hash
// and upload bytes cannot diverge when an editor saves again during transmission.
func Freeze(ctx context.Context, files []File, maxFiles int, maxBytes int64) (_ *Frozen, err error) {
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
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = in.Close()
			return nil, err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, maxBytes-total+1))
		err = errors.Join(copyErr, in.Close(), out.Close())
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxBytes {
			return nil, fmt.Errorf("snapshot exceeds %d bytes", maxBytes)
		}
		file.Source, file.Size, file.SHA256 = destination, size, hex.EncodeToString(hash.Sum(nil))
		frozen.Files = append(frozen.Files, file)
	}
	complete = true
	return frozen, nil
}
