// Package exporthistory records root-confined CLI exports. Retention only
// removes recorded files whose contents still match the published checksum.
package exporthistory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const directory = ".latexmk-cache/exports"
const maxManifest = 16 << 20

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Version int    `json:"version"`
	Files   []File `json:"files"`
}

func manifestPath(group string) string {
	digest := sha256.Sum256([]byte(group))
	return directory + "/" + hex.EncodeToString(digest[:]) + ".json"
}

func validate(file File) error {
	clean, err := safefs.Clean(file.Path)
	if err != nil || clean != file.Path ||
		(!strings.EqualFold(filepath.Ext(clean), ".pdf") && !strings.EqualFold(filepath.Ext(clean), ".zip")) {
		return errors.New("export history contains an invalid PDF/ZIP path")
	}
	digest, err := hex.DecodeString(file.SHA256)
	if err != nil || len(digest) != sha256.Size || file.Size < 0 || file.Size == 1<<63-1 {
		return errors.New("export history contains invalid content metadata")
	}
	return nil
}

func read(root *safefs.Root, name string) (manifest, error) {
	data, err := root.ReadLimited(name, maxManifest)
	if errors.Is(err, os.ErrNotExist) {
		return manifest{Version: 1}, nil
	}
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Version != 1 {
		return m, errors.New("unsupported export history version")
	}
	seen := make(map[string]bool, len(m.Files))
	for _, file := range m.Files {
		if err := validate(file); err != nil {
			return m, err
		}
		if seen[file.Path] {
			return m, errors.New("duplicate export history path")
		}
		seen[file.Path] = true
	}
	return m, nil
}

func save(root *safefs.Root, name string, m manifest) error {
	_, err := root.WriteAtomic(name, maxManifest, func(w io.Writer) error { return json.NewEncoder(w).Encode(m) })
	return err
}

// Paths reads all recorded export paths, without creating state. The atomic
// manifests can be read while another process is publishing an export.
func Paths(projectRoot string) ([]string, error) {
	root, err := safefs.Open(projectRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entries, err := root.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		m, err := read(root, directory+"/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read export history: %w", err)
		}
		for _, file := range m.Files {
			paths = append(paths, filepath.Join(projectRoot, filepath.FromSlash(file.Path)))
		}
	}
	return paths, nil
}

// Publish serializes publication and history updates across processes. write
// must publish the verified File atomically. nil keepLast retains every path.
// A successful write is recorded before pruning; failed builds never call this.
func Publish(projectRoot, group string, file File, keepLast *int, write func() error) error {
	if err := validate(file); err != nil {
		return err
	}
	if keepLast != nil && *keepLast < 1 {
		return errors.New("keepLast must be positive")
	}
	root, err := safefs.Open(projectRoot)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MakeDirs(directory, 0700); err != nil {
		return err
	}
	lock, err := root.TryLock(directory + "/.lock")
	if err != nil {
		return fmt.Errorf("lock export history; retry if another publisher is active: %w", err)
	}
	defer func() { _ = lock.Close() }()
	name := manifestPath(group)
	m, err := read(root, name)
	if err != nil {
		return err
	}
	files := make([]File, 0, len(m.Files)+1)
	for _, previous := range m.Files {
		if previous.Path != file.Path {
			files = append(files, previous)
		}
	}
	m.Files = append(files, file)
	// Check the manifest size before write so exhausting history capacity
	// cannot publish an untracked output.
	encoded, err := json.Marshal(m)
	if err != nil || len(encoded)+1 > maxManifest {
		return errors.New("export history exceeds 16 MiB")
	}
	// Stage the bookkeeping before changing the output, so an unwritable or
	// symlinked manifest cannot turn a successful export into an untracked file.
	pending, err := root.Stage(name, maxManifest, func(w io.Writer) error {
		_, err := w.Write(append(encoded, '\n'))
		return err
	})
	if err != nil {
		return err
	}
	defer func() { _ = pending.Close() }()
	otherPaths, err := otherGroupPaths(root, name)
	if err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	if err := pending.Commit(); err != nil {
		return err
	}
	published, err := root.Lstat(file.Path)
	if err != nil {
		return err
	}
	if keepLast != nil {
		latest, err := root.OpenRegular(file.Path)
		if err != nil {
			return err
		}
		verifyErr := safefs.CopyVerified(io.Discard, latest, file.Size, strings.ToLower(file.SHA256))
		if err := errors.Join(verifyErr, latest.Close()); err != nil {
			return fmt.Errorf("published export changed before retention; previous exports preserved: %w", err)
		}
	}
	// Case folding and Unicode normalization can give two spellings to one
	// directory entry. Count the just-published physical file only once.
	unique := make([]File, 0, len(m.Files))
	for _, previous := range m.Files[:len(m.Files)-1] {
		info, err := root.Lstat(previous.Path)
		if err == nil && os.SameFile(info, published) {
			continue
		}
		unique = append(unique, previous)
	}
	unique = append(unique, file)
	aliasesRemoved := len(unique) != len(m.Files)
	m.Files = unique
	if keepLast == nil || len(m.Files) <= *keepLast {
		if aliasesRemoved {
			return save(root, name, m)
		}
		return nil
	}
	cut := len(m.Files) - *keepLast
	// Two targets may deliberately publish the same path. A group's retention
	// must not remove a path still recorded by another export group.
	protected := []os.FileInfo{published}
	for _, kept := range m.Files[cut:] {
		otherPaths[kept.Path] = true
	}
	for path := range otherPaths {
		info, err := root.Lstat(path)
		if err == nil {
			protected = append(protected, info)
		}
	}
	for _, previous := range m.Files[:cut] {
		info, err := root.Lstat(previous.Path)
		if err == nil && sameAsAny(info, protected) {
			continue
		}
		if _, err := root.RemoveVerified(
			previous.Path,
			previous.Size,
			strings.ToLower(previous.SHA256),
			protected...); err != nil {
			return err
		}
	}
	m.Files = m.Files[cut:]
	return save(root, name, m)
}

func sameAsAny(info os.FileInfo, protected []os.FileInfo) bool {
	for _, other := range protected {
		if os.SameFile(info, other) {
			return true
		}
	}
	return false
}

func otherGroupPaths(root *safefs.Root, own string) (map[string]bool, error) {
	entries, err := root.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	protected := make(map[string]bool)
	for _, entry := range entries {
		name := directory + "/" + entry.Name()
		if name == own || !strings.HasSuffix(name, ".json") {
			continue
		}
		m, err := read(root, name)
		if err != nil {
			return nil, err
		}
		for _, file := range m.Files {
			protected[file.Path] = true
		}
	}
	return protected, nil
}
