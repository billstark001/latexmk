package process

import (
	"bytes"
	"sync"
)

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func newCappedBuffer(max int64) *cappedBuffer { return &cappedBuffer{max: max} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	remaining := b.max - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *cappedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *cappedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
