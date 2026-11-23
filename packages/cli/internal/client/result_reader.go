package client

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"

	"github.com/billstark001/latexmk/packages/shared/archiveutil"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const (
	maxResultEntries    = 20_000
	maxResultEntryBytes = 512 << 20
	maxResultBytes      = 1 << 30
	maxSelectedLogs     = 32
)

// resultReader applies the same envelope limits to unpacking, individual
// downloads, logs and diagnostics. Callers may stop after a verified artifact;
// callers reading to EOF also verify the complete gzip envelope.
type resultReader struct {
	*tar.Reader
	gzip       *gzip.Reader
	seen       map[string]struct{}
	totalBytes int64
}

func newResultReader(reader io.Reader) (*resultReader, error) {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return nil, invalidResult("open result gzip: %w", err)
	}
	return &resultReader{Reader: tar.NewReader(gz), gzip: gz, seen: make(map[string]struct{})}, nil
}

func (r *resultReader) Close() error { return r.gzip.Close() }

func (r *resultReader) Next() (*tar.Header, error) {
	header, err := r.Reader.Next()
	if errors.Is(err, io.EOF) {
		if err := archiveutil.VerifyTrailer(r.gzip); err != nil {
			return nil, invalidResult("verify result trailer: %w", err)
		}
		return nil, io.EOF
	}
	if err != nil {
		return nil, invalidResult("read result tar: %w", err)
	}
	if len(r.seen) >= maxResultEntries || header.Size < 0 || header.Size > maxResultEntryBytes ||
		header.Size > maxResultBytes-r.totalBytes {
		return nil, invalidResult("result archive exceeds safety limits")
	}
	if header.Typeflag != tar.TypeReg {
		return nil, invalidResult("unexpected result entry type for %q", header.Name)
	}
	if _, duplicate := r.seen[header.Name]; duplicate {
		return nil, invalidResult("result archive contains duplicate entry %q", header.Name)
	}
	r.seen[header.Name] = struct{}{}
	r.totalBytes += header.Size
	return header, nil
}

func (r *resultReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, invalidResult("read result member: %w", err)
	}
	return n, err
}

// Integrity/read failures belong to the input. Preserve destination write
// failures separately so local disk errors never become HTTP response failures.
func verifyArtifactInput(output io.Writer, input io.Reader, size int64, digest string) error {
	destination := &artifactOutput{Writer: output}
	err := safefs.CopyVerified(destination, input, size, digest)
	if err != nil && destination.err == nil {
		return invalidResult("invalid artifact input: %w", err)
	}
	return err
}

type artifactOutput struct {
	io.Writer
	err error
}

func (w *artifactOutput) Write(buffer []byte) (int, error) {
	n, err := w.Writer.Write(buffer)
	if err == nil && n != len(buffer) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}
