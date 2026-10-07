package client

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

// Rebase only source records confined to the remote project. Verification of the
// server's original artifact happens before this local, separately hashed copy.
func rebaseSyncTeX(root string, artifact protocol.Artifact, remote, local string) (protocol.Artifact, error) {
	if remote == "" || local == "" {
		return artifact, nil
	}
	file, err := projectarchive.OpenFile(
		projectarchive.File{Path: artifact.Path, Source: filepath.Join(root, filepath.FromSlash(artifact.Path))},
	)
	if err != nil {
		return artifact, err
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return artifact, err
	}
	data, err := io.ReadAll(io.LimitReader(gz, (64<<20)+1))
	err = errors.Join(err, gz.Close())
	if err != nil {
		return artifact, err
	}
	if len(data) > 64<<20 {
		return artifact, errors.New("SyncTeX expands beyond 64 MiB")
	}
	lines := strings.Split(string(data), "\n")
	remote = strings.TrimSuffix(remote, "/")
	for i, line := range lines {
		if !strings.HasPrefix(line, "Input:") {
			continue
		}
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		name := fields[2]
		if strings.HasPrefix(name, remote+"/") {
			name = strings.TrimPrefix(name, remote+"/")
		} else if path.IsAbs(name) {
			continue
		}
		name = path.Clean(name)
		if !filepath.IsLocal(filepath.FromSlash(name)) || name == "." {
			continue
		}
		lines[i] = fields[0] + ":" + fields[1] + ":" + filepath.Join(local, filepath.FromSlash(name))
	}
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	_, err = io.WriteString(writer, strings.Join(lines, "\n"))
	err = errors.Join(err, writer.Close())
	if err != nil {
		return artifact, err
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(artifact.Path)), encoded.Bytes(), 0600); err != nil {
		return artifact, err
	}
	digest := sha256.Sum256(encoded.Bytes())
	artifact.Size, artifact.SHA256 = int64(encoded.Len()), hex.EncodeToString(digest[:])
	return artifact, nil
}
