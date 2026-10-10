package client

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/billstark001/latexmk/packages/shared/archiveutil"
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
		return nil, fmt.Errorf("open result gzip: %w", err)
	}
	return &resultReader{Reader: tar.NewReader(gz), gzip: gz, seen: make(map[string]struct{})}, nil
}

func (r *resultReader) Close() error { return r.gzip.Close() }

func (r *resultReader) Next() (*tar.Header, error) {
	header, err := r.Reader.Next()
	if errors.Is(err, io.EOF) {
		if err := archiveutil.VerifyTrailer(r.gzip); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("read result tar: %w", err)
	}
	if len(r.seen) >= maxResultEntries || header.Size < 0 || header.Size > maxResultEntryBytes ||
		header.Size > maxResultBytes-r.totalBytes {
		return nil, errors.New("result archive exceeds safety limits")
	}
	if header.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("unexpected result entry type for %q", header.Name)
	}
	if _, duplicate := r.seen[header.Name]; duplicate {
		return nil, fmt.Errorf("result archive contains duplicate entry %q", header.Name)
	}
	r.seen[header.Name] = struct{}{}
	r.totalBytes += header.Size
	return header, nil
}
