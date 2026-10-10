package client

import (
	"bytes"
	"strings"
	"testing"
)

func TestRebasedSyncTeXOutputBudgetAndLineEndings(t *testing.T) {
	for _, test := range []struct {
		name, input string
		limit       int64
		output      string
		wantError   bool
	}{
		{"final newline", "Input:1:main.tex\n", 100, "Input:1:/local/main.tex\n", false},
		{"no final newline", "Input:1:main.tex", 100, "Input:1:/local/main.tex", false},
		{"empty lines", "\n\nInput:1:main.tex\n\n", 100, "\n\nInput:1:/local/main.tex\n\n", false},
		{"absolute external", "Input:1:/texmf/article.cls\n", 100, "Input:1:/texmf/article.cls\n", false},
		{"amplification", "Input:1:main.tex\n", 16, "", true},
		{"exact budget", "Input:1:main.tex", int64(len("Input:1:/local/main.tex")), "Input:1:/local/main.tex", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := writeRebasedSyncTeX(&output, []byte(test.input), "/remote", "/local", test.limit)
			if (err != nil) != test.wantError || !test.wantError && output.String() != test.output {
				t.Fatalf("output=%q, error=%v", output.String(), err)
			}
		})
	}
}

func BenchmarkRebasedSyncTeXManyLines(b *testing.B) {
	data := []byte(strings.Repeat("{1\n", 1<<18))
	for b.Loop() {
		var out bytes.Buffer
		if err := writeRebasedSyncTeX(&out, data, "/remote", "/local", maxSyncTeXExpandedBytes); err != nil {
			b.Fatal(err)
		}
	}
}
