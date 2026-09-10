package compile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Workspace owns a job's temporary directory. Reset only replaces the project
// subtree; result archives may live beside it until the response is sent.
type Workspace struct {
	Path         string
	Project      string
	parent, root *os.Root
	once         sync.Once
	closeErr     error
}

func NewWorkspace(tempDir string) (*Workspace, error) {
	dir, err := os.MkdirTemp(tempDir, "latexmk-job-*")
	if err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	root, err := parent.OpenRoot(filepath.Base(dir))
	if err != nil {
		return nil, errors.Join(err, parent.RemoveAll(filepath.Base(dir)), parent.Close())
	}
	w := &Workspace{Path: dir, Project: filepath.Join(dir, "project"), parent: parent, root: root}
	if err := w.Reset(); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

func (w *Workspace) Reset() error {
	if err := w.root.RemoveAll("project"); err != nil {
		return err
	}
	return w.root.Mkdir("project", 0o700)
}

func (w *Workspace) Close() error {
	w.once.Do(func() {
		w.closeErr = errors.Join(w.root.Close(), w.parent.RemoveAll(filepath.Base(w.Path)), w.parent.Close())
	})
	return w.closeErr
}
