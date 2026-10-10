package client

import (
	"bytes"
	"testing"
)

func TestResultReadersVerifyGzipTrailer(t *testing.T) {
	valid := buildResultArchive(t, []tarEntry{
		{name: "result.json", payload: []byte(`{"protocolVersion":2,"success":true}`)},
		{name: "stdout.log", payload: []byte("message\n")},
		{name: "stderr.log"},
	})
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 1
	for name, read := range map[string]func([]byte) error{
		"unpack": func(data []byte) error {
			var out CompileOutput
			return unpackResponse(bytes.NewReader(data), t.TempDir(), &out)
		},
		"logs": func(data []byte) error {
			_, _, err := readBoundedLogs(bytes.NewReader(data), "all", 10, 1024, 2, nil)
			return err
		},
		"diagnostics": func(data []byte) error {
			_, _, _, err := readDiagnostics(bytes.NewReader(data), nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := read(valid); err != nil {
				t.Fatalf("valid envelope: %v", err)
			}
			for name, data := range map[string][]byte{"checksum": corrupt, "truncated": valid[:len(valid)-8]} {
				t.Run(name, func(t *testing.T) {
					if err := read(data); err == nil {
						t.Fatal("accepted corrupt envelope")
					}
				})
			}
		})
	}
}
