package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"testing"
)

func TestVerifyTrailer(t *testing.T) {
	for _, test := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{name: "empty", valid: true},
		{name: "tar block padding", data: make([]byte, 10*1024), valid: true},
		{name: "padding limit", data: make([]byte, MaxTarPaddingBytes), valid: true},
		{name: "too much padding", data: make([]byte, MaxTarPaddingBytes+1)},
		{name: "hidden content", data: []byte{0, 0, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := VerifyTrailer(bytes.NewReader(test.data)); (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

func TestVerifyTrailerChecksGzipEnvelope(t *testing.T) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	valid := buffer.Bytes()
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 1
	for _, test := range []struct {
		name string
		data []byte
		want error
	}{
		{name: "valid", data: valid},
		{name: "checksum", data: corrupt, want: gzip.ErrChecksum},
		{name: "truncated", data: valid[:len(valid)-8], want: io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, err := gzip.NewReader(bytes.NewReader(test.data))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			if _, err := tar.NewReader(reader).Next(); !errors.Is(err, io.EOF) {
				t.Fatalf("tar EOF: %v", err)
			}
			if err := VerifyTrailer(reader); !errors.Is(err, test.want) {
				t.Fatalf("trailer error=%v, want %v", err, test.want)
			}
		})
	}
}
