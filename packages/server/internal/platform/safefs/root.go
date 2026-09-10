// Package safefs centralizes root-confined file access and bounded publication.
// os.Root enforces confinement during the actual operation, including concurrent
// path changes. Symlinks are additionally rejected by policy during validation.
package safefs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var ErrLimit = errors.New("file exceeds size limit")

type Root struct{ *os.Root }

func Open(root string) (*Root, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &Root{r}, nil
}

// Clean normalizes protocol paths (slash separated on every platform).
func Clean(name string) (string, error) {
	if name == "" || len(name) > 4096 || strings.ContainsAny(name, "\\\x00:") {
		return "", fmt.Errorf("invalid relative path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", fmt.Errorf("path escapes root: %q", name)
	}
	return clean, nil
}

func (r *Root) check(name string, missing bool) (string, error) {
	clean, err := Clean(name)
	if err != nil {
		return "", err
	}
	parts := strings.Split(clean, "/")
	for i := range parts {
		info, err := r.Root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if errors.Is(err, os.ErrNotExist) && missing {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink is not allowed: %s", name)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("non-directory path component: %s", name)
		}
	}
	return filepath.FromSlash(clean), nil
}

func (r *Root) MakeDirs(name string, mode os.FileMode) error {
	if name == "." {
		return nil
	}
	local, err := r.check(name, true)
	if err != nil {
		return err
	}
	return r.Root.MkdirAll(local, mode)
}

func (r *Root) OpenRegular(name string) (*os.File, error) {
	local, err := r.check(name, false)
	if err != nil {
		return nil, err
	}
	// Nonblocking open prevents a raced-in FIFO from hanging before fstat.
	f, err := r.Root.OpenFile(local, os.O_RDONLY|regularOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func (r *Root) ReadLimited(name string, max int64) ([]byte, error) {
	f, err := r.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	data, err := ReadLimited(f, max)
	return data, errors.Join(err, f.Close())
}

func ReadLimited(reader io.Reader, max int64) ([]byte, error) {
	if max < 0 || max == int64(^uint64(0)>>1) {
		return nil, errors.New("invalid read limit")
	}
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrLimit
	}
	return data, nil
}

// CopyVerified detects truncation, extra bytes and content changes in one pass.
func CopyVerified(dst io.Writer, src io.Reader, size int64, digest string) error {
	if size < 0 || size == int64(^uint64(0)>>1) {
		return errors.New("invalid content size")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("content has %d bytes; expected %d", n, size)
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("content does not match its SHA-256 digest")
	}
	return nil
}

type limitedWriter struct {
	dst                io.Writer
	remaining, written int64
	err                error
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = ErrLimit
		return 0, w.err
	}
	n, err := w.dst.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	w.err = err
	w.remaining -= int64(n)
	w.written += int64(n)
	return n, err
}

// writeFile always closes the descriptor, including when an encoder panics.
// A callback cannot suppress a write failure and publish truncated content.
func writeFile(f *os.File, max int64, syncFile bool, write func(io.Writer) error) (size int64, err error) {
	defer func() { err = errors.Join(err, f.Close()) }()
	w := &limitedWriter{dst: f, remaining: max}
	err = write(w)
	err = errors.Join(err, w.err)
	if err == nil && syncFile {
		err = f.Sync()
	}
	return w.written, err
}

func (r *Root) WriteExclusive(name string, max int64, write func(io.Writer) error) (err error) {
	if max < 0 {
		return errors.New("negative write limit")
	}
	local, err := r.check(name, true)
	if err != nil {
		return err
	}
	if err = r.MakeDirs(filepath.ToSlash(filepath.Dir(local)), 0o700); err != nil {
		return err
	}
	f, err := r.Root.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			err = errors.Join(err, r.Root.Remove(local))
		}
	}()
	_, err = writeFile(f, max, false, write)
	complete = err == nil
	return err
}

// Pending holds a fully written, synced and closed sibling file. Commit only
// renames it; a failed write or commit leaves the previous destination intact.
// Call Close on every path, including after Commit. The Root must remain open.
type Pending struct {
	root         *Root
	temp, target string
	Size         int64
}

func (r *Root) Stage(name string, max int64, write func(io.Writer) error) (_ *Pending, err error) {
	if max < 0 {
		return nil, errors.New("negative write limit")
	}
	local, err := r.check(name, true)
	if err != nil {
		return nil, err
	}
	if err := r.MakeDirs(filepath.ToSlash(filepath.Dir(local)), 0o700); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	temp := filepath.Join(filepath.Dir(local), ".latexmk-"+hex.EncodeToString(nonce[:]))
	f, err := r.Root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	pending := &Pending{root: r, temp: temp, target: local}
	complete := false
	defer func() {
		if !complete {
			err = errors.Join(err, pending.Close())
		}
	}()
	pending.Size, err = writeFile(f, max, true, write)
	if err != nil {
		return nil, err
	}
	complete = true
	return pending, nil
}

func (p *Pending) Commit() error {
	if p.temp == "" {
		return errors.New("staged file is already closed")
	}
	if _, err := p.root.check(filepath.ToSlash(p.target), true); err != nil {
		return err
	}
	if err := p.root.Root.Rename(p.temp, p.target); err != nil {
		return err
	}
	p.temp = ""
	return nil
}
func (p *Pending) Close() error {
	if p.temp == "" {
		return nil
	}
	err := p.root.Root.Remove(p.temp)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		p.temp = ""
	}
	return err
}

func (r *Root) WriteAtomic(name string, max int64, write func(io.Writer) error) (size int64, err error) {
	pending, err := r.Stage(name, max, write)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, pending.Close()) }()
	if err := pending.Commit(); err != nil {
		return 0, err
	}
	return pending.Size, nil
}
