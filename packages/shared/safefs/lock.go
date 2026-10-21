package safefs

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrLocked = errors.New("file is already locked")

// FileLock owns an advisory process lock. Close releases it; process exit also
// releases it. The lock file remains in place so all publishers use one inode.
type FileLock struct{ file *os.File }

// TryLock creates a regular lock file if needed and takes an exclusive,
// nonblocking advisory lock. It applies the same confinement, symlink and
// non-regular-file policy as other Root operations. Contention returns ErrLocked.
func (r *Root) TryLock(name string) (*FileLock, error) {
	local, err := r.check(name, true)
	if err != nil {
		return nil, err
	}
	if err := r.MakeDirs(filepath.ToSlash(filepath.Dir(local)), 0700); err != nil {
		return nil, err
	}
	file, err := regularFile(r.Root.OpenFile(local, os.O_CREATE|os.O_RDWR|regularOpenFlags, 0600))
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &FileLock{file: file}, nil
}

func (l *FileLock) Close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
