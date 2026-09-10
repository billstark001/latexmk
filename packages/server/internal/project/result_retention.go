package project

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/platform/safefs"
)

// The global retention remains an upper bound; individual requests may ask for
// a shorter lifetime. Old cache records without expiresAt keep their old TTL.
func (m *Manager) pruneRequestedCacheExpiry(now time.Time) (int64, error) {
	var reclaimed int64
	err := filepath.WalkDir(
		filepath.Join(m.stateDir, "compile-cache"),
		func(name string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(name, ".json.gz") {
				return nil
			}
			record, err := m.readCompileCache(name)
			if err != nil || record.ExpiresAt == nil || now.Before(*record.ExpiresAt) {
				return nil
			}
			info, err := os.Lstat(name)
			if err != nil {
				return err
			}
			if err := os.Remove(name); err != nil {
				return err
			}
			reclaimed += info.Size()
			return nil
		},
	)
	return reclaimed, err
}

func (m *Manager) PruneResultAuxiliary(ownerID, jobID string) error {
	name, err := m.existingResultPath(ownerID, jobID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	removed, err := m.pruneResultAuxiliary(name, time.Now())
	m.stateBytes -= removed
	return err
}

// Caller holds the storage mutex. Rewrite atomically and preserve archive age so
// expiring auxiliary files cannot extend the lifetime of PDFs and diagnostics.
func (m *Manager) pruneResultAuxiliary(name string, now time.Time) (int64, error) {
	fs, err := safefs.Open(filepath.Dir(name))
	if err != nil {
		return 0, err
	}
	defer func() { _ = fs.Close() }()
	input, err := fs.OpenRegular(filepath.Base(name))
	if err != nil {
		return 0, err
	}
	defer func() { _ = input.Close() }()
	stat, err := input.Stat()
	if err != nil {
		return 0, err
	}
	gz, err := gzip.NewReader(input)
	if err != nil {
		return 0, nil
	} // tolerate unrelated old files
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil {
		return 0, err
	}
	if header.Name != "result.json" || header.Size > 1<<20 {
		return 0, nil
	}
	var result api.CompileResult
	if err := json.NewDecoder(io.LimitReader(tr, header.Size)).Decode(&result); err != nil {
		return 0, err
	}
	if result.AuxiliaryExpiresAt == nil || now.Before(*result.AuxiliaryExpiresAt) {
		return 0, nil
	}
	drop := make(map[string]bool)
	kept := make([]api.Artifact, 0, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if artifact.Kind == "auxiliary" {
			drop["artifacts/"+artifact.Path] = true
		} else {
			kept = append(kept, artifact)
		}
	}
	if len(drop) == 0 {
		return 0, nil
	}
	result.Artifacts = kept
	payload, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	available := m.cfg.MaxStateBytes - m.stateBytes - m.pendingBytes
	size, err := fs.WriteAtomic(filepath.Base(name), available, func(w io.Writer) error {
		out := gzip.NewWriter(w)
		tw := tar.NewWriter(out)
		h := *header
		h.Size = int64(len(payload))
		if err := tw.WriteHeader(&h); err != nil {
			return err
		}
		if _, err := tw.Write(payload); err != nil {
			return err
		}
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if drop[h.Name] {
				continue
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := io.CopyN(tw, tr, h.Size); err != nil {
				return err
			}
		}
		return errors.Join(tw.Close(), out.Close())
	})
	if err != nil {
		return 0, err
	}
	if err := fs.Chtimes(filepath.Base(name), stat.ModTime(), stat.ModTime()); err != nil {
		return stat.Size() - size, err
	}
	return stat.Size() - size, nil
}
