package project

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
)

// SourceFiles describes pinned snapshot blobs for verified transport without a
// second controller-side source tree. The caller must retain the snapshot pin
// until readers finish. File.Open confines every actual read to stateDir; archive
// encoding verifies its complete size/hash before a worker can receive it.
func (m *Manager) SourceFiles(snapshot Snapshot) (map[string]compile.File, error) {
	if snapshot.OwnerID == "" || len(snapshot.Files) == 0 || len(snapshot.Files) > m.cfg.MaxFiles {
		return nil, errors.New("invalid snapshot owner or file count")
	}
	files := make(map[string]compile.File, len(snapshot.Files))
	var total int64
	for _, file := range snapshot.Files {
		if !validProjectPath(file.Path) || !validSHA256(file.SHA256) || file.Size < 0 {
			return nil, fmt.Errorf("invalid snapshot file %q", file.Path)
		}
		if _, duplicate := files[file.Path]; duplicate {
			return nil, errors.New("duplicate snapshot source path")
		}
		if file.Size > m.cfg.MaxExpandedBytes-total {
			return nil, errors.New("snapshot exceeds expanded source limit")
		}
		total += file.Size
		rel, err := filepath.Rel(m.stateDir, m.blobPath(snapshot.OwnerID, file.SHA256))
		if err != nil {
			return nil, err
		}
		files[file.Path] = compile.File{
			Workspace: m.stateDir, RelativePath: filepath.ToSlash(rel), Size: file.Size, SHA256: file.SHA256,
		}
	}
	return files, nil
}
