package project

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// StatePublication reserves both old and staged bytes. Encoding, verification
// and syncing happen outside the storage lock; only the final rename holds it.
type StatePublication struct {
	manager  *Manager
	root     *safefs.Root
	pending  *safefs.Pending
	path     string
	size     int64
	reserved bool
}

func (m *Manager) stageState(path string, size int64, write func(io.Writer) error) (_ *StatePublication, err error) {
	if size < 0 {
		return nil, errors.New("negative state publication size")
	}
	m.mu.Lock()
	available := m.cfg.MaxStateBytes - m.stateBytes - m.pendingBytes
	if size > available {
		m.mu.Unlock()
		return nil, errors.New("state storage limit prevents publication")
	}
	m.pendingBytes += size
	m.mu.Unlock()
	publication := &StatePublication{manager: m, path: path, size: size, reserved: true}
	complete := false
	defer func() {
		if !complete {
			err = errors.Join(err, publication.Close())
		}
	}()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	publication.root, err = safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	publication.pending, err = publication.root.Stage(filepath.Base(path), size, write)
	if err != nil {
		return nil, err
	}
	if publication.pending.Size != size {
		return nil, errors.New("state publication size changed")
	}
	complete = true
	return publication, nil
}

func (p *StatePublication) Commit() error {
	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()
	return p.commitLocked()
}

func (p *StatePublication) Path() string { return p.path }

func (p *StatePublication) commitLocked() error {
	if !p.reserved || p.pending == nil {
		return errors.New("state publication is already closed")
	}
	var replaced int64
	if info, err := p.root.Lstat(filepath.Base(p.path)); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("state destination is not a regular file")
		}
		replaced = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.manager.stateBytes+p.manager.pendingBytes > p.manager.cfg.MaxStateBytes {
		return errors.New("state storage limit prevents publication")
	}
	if err := p.pending.Commit(); err != nil {
		return err
	}
	p.manager.stateBytes += p.pending.Size - replaced
	p.manager.pendingBytes -= p.pending.Size
	p.reserved = false
	return nil
}

func (p *StatePublication) Close() error {
	var err error
	if p.pending != nil {
		if err := p.pending.Close(); err != nil {
			// Retain the reservation until cleanup succeeds; an undeleted
			// sibling must never silently free space in the hard quota.
			return err
		}
		p.pending = nil
	}
	if p.root != nil {
		err = errors.Join(err, p.root.Close())
		p.root = nil
	}
	if p.reserved {
		p.manager.mu.Lock()
		// The original reservation precedes Stage, so a failed Stage has no
		// Pending. Its size is kept separately from the partially written file.
		p.manager.pendingBytes -= p.size
		p.manager.mu.Unlock()
		p.reserved = false
	}
	return err
}

// Startup has no active writers. Staged siblings are never published state and
// cannot survive a process restart as valid blobs, results or checkpoints.
func removeOrphanPublications(root *safefs.Root) error {
	return filepath.WalkDir(root.Name(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !safefs.IsStagedName(entry.Name()) {
			return nil
		}
		name, err := filepath.Rel(root.Name(), path)
		if err != nil {
			return err
		}
		return root.Remove(name)
	})
}
