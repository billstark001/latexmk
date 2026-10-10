package resultarchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type testMember struct {
	name string
	data []byte
	kind byte
}

func resultEnvelope(t *testing.T, result any, members ...testMember) []byte {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	members = append([]testMember{{name: "result.json", data: raw}}, members...)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, member := range members {
		kind := member.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		if err := tw.WriteHeader(
			&tar.Header{Name: member.name, Size: int64(len(member.data)), Mode: 0600, Typeflag: kind},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	source := t.TempDir()
	data := []byte("PDF content")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(source, "main.pdf"), data, 0600); err != nil {
		t.Fatal(err)
	}
	output := compile.Output{
		Result: protocol.CompileResult{
			ProtocolVersion: 2,
			Success:         true,
			Artifacts:       []protocol.Artifact{{Path: "main.pdf", Size: int64(len(data)), SHA256: digest}},
		},
		Stdout: []byte("out"),
		Stderr: []byte("err"),
		Files:  []compile.File{{Workspace: source, RelativePath: "main.pdf", Size: int64(len(data)), SHA256: digest}},
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, output); err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(
		bytes.NewReader(encoded.Bytes()),
		t.TempDir(),
		Limits{MaxFiles: 1, MaxArtifacts: int64(len(data)), MaxLogs: 3},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Result.Success || string(decoded.Stdout) != "out" || string(decoded.Stderr) != "err" ||
		len(decoded.Files) != 1 {
		t.Fatalf("output = %+v", decoded)
	}
	actual, err := os.ReadFile(filepath.Join(decoded.Files[0].Workspace, decoded.Files[0].RelativePath))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("artifact = %q, %v", actual, err)
	}
	var second bytes.Buffer
	if err := Encode(&second, output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded.Bytes(), second.Bytes()) {
		t.Fatal("encoding is not deterministic")
	}
}

func TestDecodeRejectsInvalidEnvelope(t *testing.T) {
	sum := sha256.Sum256(nil)
	digest := hex.EncodeToString(sum[:])
	logs := []testMember{{name: "stdout.log"}, {name: "stderr.log"}}
	for _, test := range []struct {
		name    string
		result  any
		members []testMember
		limits  Limits
		want    string
	}{
		{"null metadata", nil, logs, Limits{10, 100, 100}, "invalid result"},
		{"missing logs", protocol.CompileResult{}, nil, Limits{10, 100, 100}, "incomplete"},
		{"duplicate member", protocol.CompileResult{}, append(append([]testMember{}, logs...), logs[0]), Limits{10, 100, 100}, "invalid result archive"},
		{"undeclared artifact", protocol.CompileResult{}, append(append([]testMember{}, logs...), testMember{name: "artifacts/main.pdf"}), Limits{10, 100, 100}, "undeclared"},
		{"traversal", protocol.CompileResult{Artifacts: []protocol.Artifact{{Path: "../escape", SHA256: digest}}}, logs, Limits{10, 100, 100}, "metadata"},
		{"non hex digest", protocol.CompileResult{Artifacts: []protocol.Artifact{{Path: "main.pdf", SHA256: strings.Repeat("z", 64)}}}, logs, Limits{10, 100, 100}, "metadata"},
		{"overflow total", protocol.CompileResult{Artifacts: []protocol.Artifact{{Path: "one", Size: math.MaxInt64 - 1, SHA256: digest}, {Path: "two", Size: 2, SHA256: digest}}}, logs, Limits{10, math.MaxInt64, 100}, "limits"},
		{"negative limits", protocol.CompileResult{}, logs, Limits{-1, 100, 100}, "limits"},
		{"oversized log", protocol.CompileResult{}, []testMember{{name: "stdout.log", data: []byte("xx")}, {name: "stderr.log"}}, Limits{10, 100, 1}, "limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(resultEnvelope(t, test.result, test.members...)), t.TempDir(), test.limits)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v; expected %q", err, test.want)
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	var valid bytes.Buffer
	if err := Encode(&valid, compile.Output{}); err != nil {
		f.Fatal(err)
	}
	f.Add(valid.Bytes())
	f.Add([]byte("not gzip"))
	f.Add([]byte{0x1f, 0x8b, 8, 0})
	destination := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		// No artifact may be published, so all iterations can reuse the same root.
		_, _ = Decode(bytes.NewReader(data), destination, Limits{MaxLogs: 1024})
	})
}
