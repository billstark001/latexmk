package project

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func (m *Manager) LiveCachePath(ownerID, sessionID string) (string, error) {
	if ownerID == "" || !validProjectID(sessionID) {
		return "", errors.New("invalid live cache identity")
	}
	dir := filepath.Join(m.stateDir, "live-cache", ownerKey(ownerID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID+".tar.gz"), nil
}

func (m *Manager) SaveLiveCache(ownerID, sessionID, source string, size int64, digest string) (string, error) {
	path, err := m.LiveCachePath(ownerID, sessionID)
	if err != nil {
		return "", err
	}
	inputFS, err := safefs.Open(filepath.Dir(source))
	if err != nil {
		return "", err
	}
	defer func() { _ = inputFS.Close() }()
	input, err := inputFS.OpenRegular(filepath.Base(source))
	if err != nil {
		return "", err
	}
	defer func() { _ = input.Close() }()
	m.mu.Lock()
	defer m.mu.Unlock()
	destination, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = destination.Close() }()
	var previous int64
	if info, err := destination.Lstat(filepath.Base(path)); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("live cache is not a regular file")
		}
		previous = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	available := min(m.cfg.MaxCompileCacheBytes+(1<<20), m.cfg.MaxStateBytes-m.stateBytes-m.pendingBytes)
	written, err := destination.WriteAtomic(
		filepath.Base(path),
		available,
		func(w io.Writer) error { return safefs.CopyVerified(w, input, size, digest) },
	)
	if err != nil {
		return "", err
	}
	m.stateBytes += written - previous
	return path, nil
}

func (m *Manager) DeleteLiveCache(ownerID, sessionID string) error {
	path, err := m.LiveCachePath(ownerID, sessionID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("live cache is not a regular file")
	}
	if err := root.Remove(filepath.Base(path)); err != nil {
		return err
	}
	m.stateBytes -= info.Size()
	return nil
}
