package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ExpectSize returns r limited to exactly size bytes, for a driver
// enforcing PutOptions.Size: reading past size, or reaching EOF before it,
// fails with ErrSizeMismatch. A size of zero or less returns r unchanged.
func ExpectSize(r io.Reader, size int64) io.Reader {
	if size <= 0 {
		return r
	}
	return &exactReader{r: r, left: size, size: size}
}

type exactReader struct {
	r          io.Reader
	left, size int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left == 0 {
		// The declared length is used up: the source must be at EOF.
		var one [1]byte
		n, err := e.r.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: more than %d bytes", ErrSizeMismatch, e.size)
		}
		if err == nil {
			// A reader may return 0, nil; ask again on the next call.
			return 0, nil
		}
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, err
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if errors.Is(err, io.EOF) && e.left > 0 {
		return n, fmt.Errorf("%w: %d bytes, declared %d", ErrSizeMismatch, e.size-e.left, e.size)
	}
	if errors.Is(err, io.EOF) {
		// Exactly size bytes: EOF is right, but report it on the next Read
		// so a caller that stops at the size still sees any excess.
		return n, nil
	}
	return n, err
}

// ContextReader returns r stopping at ctx: each Read first checks ctx and
// fails with its error once it has ended. A driver copies a Put's reader
// through it so a canceled Put stops streaming instead of reading to EOF.
func ContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
