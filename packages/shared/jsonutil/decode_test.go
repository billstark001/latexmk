package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func TestDecodeEnvelope(t *testing.T) {
	for _, test := range []struct {
		name, body string
		limit      int64
		wantErr    bool
	}{
		{"exact budget", `{"name":"ok"}`, 13, false},
		{"whitespace", " {\"name\":\"ok\"}\n", 15, false},
		{"too small", `{"name":"ok"}`, 12, true},
		{"trailing whitespace counts", "{} ", 2, true},
		{"empty", "", 0, true},
		{"null", " null\n", 64, true},
		{"multiple", "{} {}", 64, true},
		{"garbage", "{} garbage", 64, true},
		{"wrong type", "[]", 64, true},
		{"negative budget", "{}", -1, true},
		{"overflow budget", "{}", math.MaxInt64, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var destination struct {
				Name string `json:"name"`
			}
			err := Decode(strings.NewReader(test.body), test.limit, &destination)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
	var destination struct {
		Name string `json:"name"`
	}
	if err := Decode(strings.NewReader(`{"future":true}`), 64, &destination); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(strings.NewReader(`{"future":true}`), 64, &destination); err == nil {
		t.Fatal("accepted unknown field")
	}
	if err := Decode(strings.NewReader("{} "), 2, &destination); !errors.Is(err, safefs.ErrLimit) {
		t.Fatalf("limit error = %v", err)
	}
	if err := Decode(failingReader{}, 64, &destination); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func FuzzDecodeBoundedEnvelope(f *testing.F) {
	for _, payload := range []string{`{"name":"ok"}`, `null`, `{} {}`, `{"future":true}`, `{"name":"你好"}`, "{}\n"} {
		f.Add([]byte(payload), uint16(128), true)
	}
	f.Fuzz(func(t *testing.T, payload []byte, budget uint16, strict bool) {
		limit := int64(budget % 1025)
		reader := &countingReader{reader: bytes.NewReader(payload)}
		var result struct {
			Name string `json:"name"`
		}
		var err error
		if strict {
			err = DecodeStrict(reader, limit, &result)
		} else {
			err = Decode(reader, limit, &result)
		}
		if reader.bytes > limit+1 {
			t.Fatalf("read %d bytes with budget %d", reader.bytes, limit)
		}
		if err == nil &&
			(int64(len(payload)) > limit || !json.Valid(payload) || bytes.Equal(bytes.TrimSpace(payload), []byte("null"))) {
			t.Fatalf("accepted invalid envelope %q under budget %d", payload, limit)
		}
	})
}

type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}
