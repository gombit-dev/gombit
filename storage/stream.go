package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ExpectSize returns r limited to exactly size bytes, for a driver
// enforcing PutOptions.Size: a source that ends early, or has a byte past
// size, fails with ErrSizeMismatch, and a size of zero means an empty
// source. The read that reaches size looks one byte further, so the
// mismatch comes with it: read the result to EOF (io.Copy, io.ReadAll).
// io.ReadFull and io.CopyN stop at a byte count and can drop an error
// returned with the last bytes. A negative size fails every read with
// ErrInvalidOptions.
func ExpectSize(r io.Reader, size int64) io.Reader {
	return &exactReader{r: r, left: size, size: size}
}

type exactReader struct {
	r          io.Reader
	left, size int64
	done       error // the result once size is reached: io.EOF, or why not
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.done != nil {
		return 0, e.done
	}
	if e.size < 0 {
		e.done = fmt.Errorf("%w: negative size %d", ErrInvalidOptions, e.size)
		return 0, e.done
	}
	n := 0
	if e.left > 0 {
		if int64(len(p)) > e.left {
			p = p[:e.left]
		}
		var err error
		n, err = e.r.Read(p)
		e.left -= int64(n)
		switch {
		case e.left > 0 && errors.Is(err, io.EOF):
			e.done = fmt.Errorf("%w: %d bytes, declared %d", ErrSizeMismatch, e.size-e.left, e.size)
			return n, e.done
		case e.left > 0 || (err != nil && !errors.Is(err, io.EOF)):
			return n, err
		case errors.Is(err, io.EOF):
			// Exactly size bytes and the source says it is done.
			e.done = io.EOF
			return n, io.EOF
		}
	}
	// size bytes have been read and the source has not said it is done:
	// look one byte further, so any excess is reported now.
	e.done = e.probe()
	return n, e.done
}

// probe reads past the declared size: io.EOF when the source is done,
// ErrSizeMismatch when it has another byte.
func (e *exactReader) probe() error {
	var one [1]byte
	for tries := 0; tries < 100; tries++ {
		n, err := e.r.Read(one[:])
		switch {
		case n > 0:
			return fmt.Errorf("%w: more than %d bytes", ErrSizeMismatch, e.size)
		case errors.Is(err, io.EOF):
			return io.EOF
		case err != nil:
			return err
		}
	}
	return io.ErrNoProgress
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

// PutReader returns r as a driver's Put should read it: limited to
// opts.Size when one is declared (ExpectSize), and stopping when ctx ends
// (ContextReader). Read it to EOF.
func PutReader(ctx context.Context, r io.Reader, opts PutOptions) io.Reader {
	if opts.Size != nil {
		r = ExpectSize(r, *opts.Size)
	}
	return ContextReader(ctx, r)
}

// ContextReadCloser returns rc bound to ctx, as the reader Storage.Open
// returns must be: once ctx has ended, Read fails with its error, and a
// Read already in progress is interrupted, by closing rc (the way an HTTP
// response body behaves when its request's context ends). Close closes rc
// once. When rc can seek, so can the result.
func ContextReadCloser(ctx context.Context, rc io.ReadCloser) io.ReadCloser {
	c := &ctxReadCloser{ctx: ctx, rc: rc}
	c.stop = context.AfterFunc(ctx, func() { _ = c.close() })
	if s, ok := rc.(io.Seeker); ok {
		return &ctxReadSeekCloser{c, s}
	}
	return c
}

type ctxReadCloser struct {
	ctx    context.Context
	rc     io.ReadCloser
	stop   func() bool
	once   sync.Once
	closed error
}

func (c *ctxReadCloser) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.rc.Read(p)
	if cerr := c.ctx.Err(); cerr != nil {
		// Ended during the Read: whatever the interrupted read returned,
		// the stream is over with the context's error.
		return 0, cerr
	}
	return n, err
}

func (c *ctxReadCloser) close() error {
	c.once.Do(func() { c.closed = c.rc.Close() })
	return c.closed
}

func (c *ctxReadCloser) Close() error {
	c.stop()
	return c.close()
}

type ctxReadSeekCloser struct {
	*ctxReadCloser
	io.Seeker
}
