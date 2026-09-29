package storagetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
)

// fake is a minimal in-memory reference driver that keeps the contract.
// The flags break it one way each, to show the suite catches that.
type fake struct {
	mu      sync.Mutex
	objects map[string]object

	nonAtomic        bool // stores what it read so far even when the Put fails
	noKeyCheck       bool // skips ValidateKey
	ignoresSize      bool // skips PutOptions.Size
	ignoresCtx       bool // never checks ctx while streaming
	deleteMissingErr bool // Delete of a missing key is ErrNotFound
	noDefaultType    bool // stores "" as the content type
	emptyURL         bool // URL returns "", nil
	dropsMetadata    bool // does not keep PutOptions.Metadata
}

type object struct {
	data []byte
	info storage.ObjectInfo
}

func newFake() *fake { return &fake{objects: map[string]object{}} }

func (f *fake) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if err := f.checkKey(key); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	if err := storage.ValidatePutOptions(opts); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	if !f.ignoresSize {
		r = storage.ExpectSize(r, opts.Size)
	}
	info := storage.ObjectInfo{Key: key, ContentType: opts.ContentType, ModTime: time.Now()}
	if info.ContentType == "" && !f.noDefaultType {
		info.ContentType = storage.DefaultContentType
	}
	if !f.dropsMetadata && len(opts.Metadata) > 0 {
		info.Metadata = make(map[string]string, len(opts.Metadata))
		for k, v := range opts.Metadata {
			info.Metadata[k] = v
		}
	}
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		if !f.ignoresCtx {
			if err := ctx.Err(); err != nil {
				return storage.ObjectInfo{}, storage.Wrap("put", key, err)
			}
		}
		n, err := r.Read(chunk)
		buf.Write(chunk[:n])
		if f.nonAtomic {
			f.store(key, buf.Bytes(), info)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return storage.ObjectInfo{}, storage.Wrap("put", key, err)
		}
	}
	info.Size = int64(buf.Len())
	f.store(key, buf.Bytes(), info)
	return info, nil
}

func (f *fake) store(key string, data []byte, info storage.ObjectInfo) {
	info.Size = int64(len(data))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = object{data: append([]byte(nil), data...), info: info}
}

func (f *fake) checkKey(key string) error {
	if f.noKeyCheck {
		return nil
	}
	return storage.ValidateKey(key)
}

func (f *fake) get(op, key string) (object, error) {
	if err := f.checkKey(key); err != nil {
		return object{}, storage.Wrap(op, key, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return object{}, storage.Wrap(op, key, storage.ErrNotFound)
	}
	return o, nil
}

func (f *fake) Open(_ context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	o, err := f.get("open", key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	return io.NopCloser(bytes.NewReader(o.data)), o.info, nil
}

func (f *fake) Stat(_ context.Context, key string) (storage.ObjectInfo, error) {
	o, err := f.get("stat", key)
	return o.info, err
}

func (f *fake) Delete(_ context.Context, key string) error {
	if err := f.checkKey(key); err != nil {
		return storage.Wrap("delete", key, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok && f.deleteMissingErr {
		return storage.Wrap("delete", key, storage.ErrNotFound)
	}
	delete(f.objects, key)
	return nil
}

func (f *fake) URL(_ context.Context, key string, opts storage.URLOptions) (string, error) {
	if err := f.checkKey(key); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := storage.ValidateURLOptions(opts); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if f.emptyURL {
		return "", nil
	}
	return "", storage.Wrap("url", key, storage.ErrUnsupported)
}

func TestReferenceFakePasses(t *testing.T) {
	Run(t, func(*testing.T) storage.Storage { return newFake() })
}

// recorder captures a check's failures; Fatal ends the check's goroutine.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	failed []string
}

func (r *recorder) Helper() {}

func (r *recorder) Cleanup(func()) {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Fatal(args ...any) {
	r.Errorf("%s", fmt.Sprint(args...))
	runtime.Goexit()
}

// runCheck runs the named check against s and returns its failures.
func runCheck(t *testing.T, name string, s storage.Storage) []string {
	t.Helper()
	for _, c := range checks {
		if c.name != name {
			continue
		}
		rec := &recorder{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.run(rec, s)
		}()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatalf("check %s hung", name)
		}
		return rec.failed
	}
	t.Fatalf("no check %s", name)
	return nil
}

// TestSuiteCatchesBrokenDrivers: each way of breaking the contract fails
// the check that covers it, so a driver that passes the suite keeps it.
func TestSuiteCatchesBrokenDrivers(t *testing.T) {
	cases := []struct {
		check  string
		break_ func(*fake)
		want   string
	}{
		{"FailedPutKeepsPrevious", func(f *fake) { f.nonAtomic = true }, "want the previous version whole"},
		{"InvalidKeys", func(f *fake) { f.noKeyCheck = true }, "want storage.ErrInvalidKey"},
		{"SizeMismatch", func(f *fake) { f.ignoresSize = true }, "want storage.ErrSizeMismatch"},
		{"CanceledPut", func(f *fake) { f.ignoresCtx = true }, "want context.Canceled"},
		{"Missing", func(f *fake) { f.deleteMissingErr = true }, "deletes are idempotent"},
		{"DefaultContentType", func(f *fake) { f.noDefaultType = true }, "want \"application/octet-stream\""},
		{"URL", func(f *fake) { f.emptyURL = true }, "empty URL and no error"},
		{"RoundTrip", func(f *fake) { f.dropsMetadata = true }, "metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.check, func(t *testing.T) {
			f := newFake()
			tc.break_(f)
			failed := runCheck(t, tc.check, f)
			if !strings.Contains(strings.Join(failed, "\n"), tc.want) {
				t.Fatalf("the %s check did not catch the broken driver; it reported %q", tc.check, failed)
			}
			if clean := runCheck(t, tc.check, newFake()); len(clean) != 0 {
				t.Fatalf("the %s check fails the correct driver: %q", tc.check, clean)
			}
		})
	}
}
