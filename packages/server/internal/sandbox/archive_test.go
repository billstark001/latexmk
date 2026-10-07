package sandbox

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

type byteCounter struct{ count int64 }

func (w *byteCounter) Write(data []byte) (int, error) {
	w.count += int64(len(data))
	return len(data), nil
}

// BenchmarkArchiveCompression models local container transport, including the
// same regular-file checks and digest verification used by production archives.
func BenchmarkArchiveCompression(b *testing.B) {
	root := b.TempDir()
	text := bytes.Buffer{}
	for i := range 12000 {
		fmt.Fprintf(&text, "\\newlabel{label-%d}{{%d}{%d}}\n", i, i%127, i%63)
	}
	rng := rand.New(rand.NewPCG(17, 31))
	binary := make([]byte, 4<<20)
	for i := range binary {
		binary[i] = byte(rng.Uint32())
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(binary); err != nil {
		b.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		b.Fatal(err)
	}
	fixtures := map[string][]byte{
		"labels.aux": text.Bytes(), "figure.bin": binary, "checkpoint.tar.gz": compressed.Bytes(),
	}
	files := make(map[string]archiveMember)
	for name, data := range fixtures {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			b.Fatal(err)
		}
		file, err := describeFile(root, name, int64(len(data)))
		if err != nil {
			b.Fatal(err)
		}
		files[name] = archiveMember{name: name, file: file}
	}
	resultMember := files["checkpoint.tar.gz"]
	resultMember.name = "result.tar.gz"
	cases := []struct {
		name    string
		members []archiveMember
	}{
		{"text-state", []archiveMember{files["labels.aux"]}},
		{"checkpoint", []archiveMember{files["labels.aux"], files["figure.bin"]}},
		{"nested-envelope", []archiveMember{resultMember, files["checkpoint.tar.gz"]}},
	}
	for _, fixture := range cases {
		for _, level := range []struct {
			name  string
			value int
		}{{"default", gzip.DefaultCompression}, {"fast", gzip.BestSpeed}, {"stored", gzip.NoCompression}} {
			b.Run(fixture.name+"/"+level.name, func(b *testing.B) {
				for b.Loop() {
					var output byteCounter
					if err := writeArchive(&output, fixture.members, level.value); err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(output.count), "compressed-bytes/op")
				}
			})
		}
	}
}
