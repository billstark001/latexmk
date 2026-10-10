// Package archiveutil provides bounded validation for tar stream envelopes.
package archiveutil

import (
	"fmt"
	"io"
)

// MaxTarPaddingBytes permits conventional tar blocking padding while bounding
// work on untrusted data after the end-of-archive marker.
const MaxTarPaddingBytes = 1 << 20

// VerifyTrailer consumes the underlying stream after tar.Reader reports EOF.
// Only zero padding is allowed. Reading through EOF also checks a gzip.Reader's
// checksum and detects truncation, which closing that reader does not do.
// The caller owns the reader and must still close it when appropriate.
func VerifyTrailer(reader io.Reader) error {
	var buffer [32 << 10]byte
	var total int64
	limited := io.LimitReader(reader, MaxTarPaddingBytes+1)
	for {
		n, err := limited.Read(buffer[:])
		total += int64(n)
		if total > MaxTarPaddingBytes {
			return fmt.Errorf("tar padding exceeds %d bytes", MaxTarPaddingBytes)
		}
		for _, value := range buffer[:n] {
			if value != 0 {
				return fmt.Errorf("unexpected data after tar end marker")
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive trailer: %w", err)
		}
	}
}
