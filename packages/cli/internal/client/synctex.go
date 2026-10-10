package client

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const maxSyncTeXExpandedBytes = 64 << 20

// Rebase only source records confined to the remote project. Verification of the
// server's original artifact happens before this local, separately hashed copy.
func rebaseSyncTeX(root string, artifact protocol.Artifact, remote, local string) (protocol.Artifact, error) {
	if remote == "" || local == "" {
		return artifact, nil
	}
	if strings.ContainsAny(local, "\r\n\x00") {
		return artifact, errors.New("local source path cannot be represented in SyncTeX")
	}
	scoped, err := safefs.Open(root)
	if err != nil {
		return artifact, err
	}
	defer func() { _ = scoped.Close() }()
	file, err := scoped.OpenRegular(artifact.Path)
	if err != nil {
		return artifact, err
	}
	gz, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return artifact, err
	}
	data, readErr := safefs.ReadLimited(gz, maxSyncTeXExpandedBytes)
	err = errors.Join(readErr, gz.Close(), file.Close())
	if err != nil {
		return artifact, err
	}
	hash := sha256.New()
	size, err := scoped.WriteAtomic(artifact.Path, maxResultEntryBytes, func(out io.Writer) error {
		writer := gzip.NewWriter(io.MultiWriter(out, hash))
		err := writeRebasedSyncTeX(writer, data, remote, local, maxSyncTeXExpandedBytes)
		return errors.Join(err, writer.Close())
	})
	if err != nil {
		return artifact, err
	}
	artifact.Size, artifact.SHA256 = size, hex.EncodeToString(hash.Sum(nil))
	return artifact, nil
}

// Scan one line at a time without allocating a string table for every source
// line. The output budget also bounds amplification from long local roots.
func writeRebasedSyncTeX(w io.Writer, data []byte, remote, local string, remaining int64) error {
	buffered := bufio.NewWriterSize(w, 32<<10)
	remote = strings.TrimSuffix(remote, "/")
	for len(data) > 0 {
		line, rest, newline := bytes.Cut(data, []byte{'\n'})
		segment := line
		if newline {
			segment = data[:len(line)+1]
		}
		data = rest
		if bytes.HasPrefix(line, []byte("Input:")) {
			fields := strings.SplitN(string(line), ":", 3)
			if len(fields) == 3 {
				name := fields[2]
				if strings.HasPrefix(name, remote+"/") {
					name = strings.TrimPrefix(name, remote+"/")
				} else if path.IsAbs(name) {
					name = ""
				}
				name = path.Clean(name)
				if name != "." && filepath.IsLocal(filepath.FromSlash(name)) {
					replacement := fields[0] + ":" + fields[1] + ":" + filepath.Join(local, filepath.FromSlash(name))
					if newline {
						replacement += "\n"
					}
					segment = []byte(replacement)
				}
			}
		}
		size := int64(len(segment))
		if size > remaining {
			return errors.New("rebased SyncTeX expands beyond its byte limit")
		}
		remaining -= size
		if _, err := buffered.Write(segment); err != nil {
			return err
		}

	}
	return buffered.Flush()
}
