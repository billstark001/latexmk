package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type LivePublication struct {
	SessionID        string              `json:"sessionId"`
	Revision         uint64              `json:"revision"`
	JobID            string              `json:"jobId"`
	SourceRoot       string              `json:"sourceRoot"`
	RemoteSourceRoot string              `json:"remoteSourceRoot,omitempty"`
	SnapshotID       string              `json:"snapshotId"`
	Directory        string              `json:"directory"`
	Artifacts        []protocol.Artifact `json:"artifacts"`
}

// DownloadLiveResult validates into an immutable generation. Only a successful
// bundle advances current.json/current, so errors never replace the last good PDF.
func (c *Client) DownloadLiveResult(
	ctx context.Context,
	job protocol.Job,
	request protocol.CompileRequest,
	outputRoot string,
) (CompileOutput, string, error) {
	var out CompileOutput
	if job.Result == nil {
		return out, "", &ResultStateError{Status: job.Status}
	}
	key := sha256.Sum256([]byte(request.Entry + "\x00" + request.Engine + "\x00" + request.JobName))
	rootAbs, err := filepath.Abs(outputRoot)
	if err != nil {
		return out, "", err
	}
	if err := os.MkdirAll(rootAbs, 0700); err != nil {
		return out, "", err
	}
	root, err := ensureSafeParent(rootAbs, filepath.Join(".latexmk-live", hex.EncodeToString(key[:8])))
	if err != nil {
		return out, "", err
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return out, "", err
	}
	defer func() { _ = fs.Close() }()
	generation, err := os.MkdirTemp(root, "generation-")
	if err != nil {
		return out, "", err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(generation)
		}
	}()
	resp, err := c.resultResponse(ctx, job.ID)
	if err != nil {
		return out, "", err
	}
	decodeErr := unpackResponseWithPolicy(resp.Body, generation, &out, request, c.ProjectRoot)
	err = errors.Join(decodeErr, resp.Body.Close())
	if err != nil {
		return out, "", err
	}
	if out.Result.RequestID != job.ID || out.Result.SessionID != job.SessionID || out.Result.Revision != job.Revision {
		return out, "", errors.New("result does not match the requested session revision")
	}
	out.Result.CompileCache = job.Result.CompileCache
	if !out.Result.Success {
		return out, "", nil
	}
	// A normal result directory and exported PDF are still supported through the
	// existing export operation; publication itself is a versioned bundle.
	if err := ctx.Err(); err != nil {
		return out, "", err
	}
	var localArtifacts []protocol.Artifact
	for _, artifact := range out.Result.Artifacts {
		if info, err := os.Lstat(
			filepath.Join(generation, filepath.FromSlash(artifact.Path)),
		); err == nil &&
			info.Mode().IsRegular() {
			if strings.HasSuffix(artifact.Path, ".synctex.gz") {
				artifact, err = rebaseSyncTeX(generation, artifact, out.Result.SourceRoot, c.ProjectRoot)
				if err != nil {
					return out, "", err
				}
			}
			localArtifacts = append(localArtifacts, artifact)
		}
	}
	manifest := LivePublication{
		SessionID:        job.SessionID,
		Revision:         job.Revision,
		JobID:            job.ID,
		SourceRoot:       c.ProjectRoot,
		RemoteSourceRoot: out.Result.SourceRoot,
		SnapshotID:       job.SnapshotID,
		Directory:        filepath.Base(generation),
		Artifacts:        localArtifacts,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return out, "", err
	}
	writeManifest := func(writer io.Writer) error {
		_, err := io.Copy(writer, bytes.NewReader(raw))
		return err
	}
	if err := fs.WriteExclusive(
		filepath.Base(generation)+"/publication.json",
		int64(len(raw)),
		writeManifest,
	); err != nil {
		return out, "", err
	}
	previous := LivePublication{}
	if data, err := fs.ReadLimited("current.json", 1<<20); err == nil {
		if err := json.Unmarshal(data, &previous); err != nil {
			return out, "", err
		}
		if previous.SessionID == job.SessionID && (previous.Revision > job.Revision ||
			(previous.Revision == job.Revision && previous.JobID != job.ID)) {
			return out, "", errors.New("refusing to publish an older or conflicting session revision")
		}
	} else if !errors.Is(
		err,
		os.ErrNotExist,
	) {
		return out, "", err
	}
	if runtime.GOOS != "windows" {
		if info, err := fs.Lstat("current"); err == nil && info.Mode()&os.ModeSymlink == 0 {
			return out, "", errors.New("live current pointer is not a managed symlink")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, "", err
		}
	}
	if _, err := fs.WriteAtomic("current.json", int64(len(raw)), writeManifest); err != nil {
		return out, "", err
	}
	published = true
	if runtime.GOOS != "windows" {
		if info, err := fs.Lstat("current"); err == nil && info.Mode()&os.ModeSymlink == 0 {
			return out, generation, errors.New("live current pointer is not a managed symlink")
		}
		link := ".current-" + filepath.Base(generation)
		if err := fs.Symlink(filepath.Base(generation), link); err != nil {
			return out, generation, err
		}
		if err := fs.Rename(link, "current"); err != nil {
			_ = fs.Remove(link)
			return out, generation, err
		}
	}
	dir, err := fs.Open(".")
	if err != nil {
		return out, generation, err
	}
	entries, readErr := dir.ReadDir(-1)
	err = errors.Join(readErr, dir.Close())
	if err != nil {
		return out, generation, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "generation-") ||
			entry.Name() == filepath.Base(generation) ||
			entry.Name() == previous.Directory {
			continue
		}
		// Generation names came from MkdirTemp; no user or server path is traversed.
		if err := fs.RemoveAll(entry.Name()); err != nil {
			return out, generation, fmt.Errorf("remove old live generation: %w", err)
		}
	}
	return out, generation, nil
}
