// Package resultarchive produces the stable result artifact consumed by the
// CLI. It is shared by the legacy synchronous endpoint and queued jobs.
package resultarchive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"path/filepath"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/platform/safefs"
)

func Write(path string, output compile.Output) error {
	fs, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = fs.Close() }()
	return fs.WriteExclusive(
		filepath.Base(path),
		int64(^uint64(0)>>1),
		func(w io.Writer) error { return Encode(w, output) },
	)
}

// Encode writes the archive to a caller-owned writer. Publication and quota
// enforcement belong to the storage layer.
func Encode(w io.Writer, output compile.Output) error {
	gz := gzip.NewWriter(w)
	gz.Name = ""
	gz.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	writeBytes := func(name string, data []byte) error {
		header := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			ModTime:  time.Unix(0, 0),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	resultJSON, err := json.MarshalIndent(output.Result, "", "  ")
	if err != nil {
		return err
	}
	resultJSON = append(resultJSON, '\n')
	if err := writeBytes("result.json", resultJSON); err != nil {
		return err
	}
	if err := writeBytes("stdout.log", output.Stdout); err != nil {
		return err
	}
	if err := writeBytes("stderr.log", output.Stderr); err != nil {
		return err
	}
	for _, artifact := range output.Files {
		in, err := artifact.Open()
		if err != nil {
			return err
		}
		header := &tar.Header{
			Name:     "artifacts/" + filepath.ToSlash(artifact.RelativePath),
			Mode:     0o644,
			Size:     artifact.Size,
			ModTime:  time.Unix(0, 0),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			_ = in.Close()
			return err
		}
		copyErr := safefs.CopyVerified(tw, in, artifact.Size, artifact.SHA256)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return nil
}
