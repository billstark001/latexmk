package safefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelAfterRead) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.cancel()
	return n, err
}

func TestCancellationStopsDigestBeforeReadingTheRemainingFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := bytes.Repeat([]byte("x"), 1<<20)
	_, size, err := Digest(WithContext(ctx, cancelAfterRead{bytes.NewReader(data), cancel}), int64(len(data)))
	if !errors.Is(err, context.Canceled) || size <= 0 || size >= int64(len(data)) {
		t.Fatalf("cancellation consumed the remaining source: bytes=%d, error=%v", size, err)
	}
}
