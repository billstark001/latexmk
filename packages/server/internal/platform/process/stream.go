package process

import (
	"context"
	"errors"
	"io"
)

var ErrStreamLimit = errors.New("subprocess output exceeds stream limit")

type streamWriter struct {
	dst       io.Writer
	remaining int64
	cancel    context.CancelFunc
	err       error
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = ErrStreamLimit
		w.cancel()
		return 0, w.err
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}
