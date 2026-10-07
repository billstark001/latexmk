package safefs

import (
	"context"
	"io"
)

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// WithContext checks cancellation between reads. It cannot interrupt an
// underlying Read already in progress; filesystem callers open regular files.
func WithContext(ctx context.Context, reader io.Reader) io.Reader {
	return contextReader{ctx: ctx, reader: reader}
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}
