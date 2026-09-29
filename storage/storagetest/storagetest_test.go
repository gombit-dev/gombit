package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
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
	keepsOldType     bool // an overwrite keeps the previous content type
	deleteByPrefix   bool // Delete also removes keys under key + "/"
	baseNameKey      bool // reports path.Base(key) as ObjectInfo.Key
	truncates        bool // stores at most 1 MiB, reading the rest
	rejectsEmpty     bool // refuses a zero-length object
	noOptionsCheck   bool // skips ValidatePutOptions
	tornWrites       bool // overwrites in place, only every other chunk
	readIgnoresCtx   bool // Open/Stat/Delete/URL never check ctx
	caseInsensitive  bool // keys are compared case-insensitively
	urlNeedsObject   bool // URL returns ErrNotFound for a missing object
	anyExpiry        bool // URL accepts a lifetime over MaxURLExpiry
	signedNotPublic  bool // URL refuses a signed URL for a private object
	bareErrors       bool // returns bare sentinels, not *storage.Error
	aliasMetadata    bool // stores and returns one shared metadata map
	openDetached     bool // Open's reader ignores the context once returned
	nonAtomicFirst   bool // a key's first Put writes in place as it streams
	resultIsInput    bool // Put's result carries the caller's metadata map
	constantETag     bool // every version reports the same ETag
	putNoETag        bool // Put reports no ETag (Open and Stat do)
	openNoETag       bool // Open reports no ETag (Put and Stat do)
	keepsForeignErr  bool // wraps like the old Wrap: keeps any *storage.Error
	eofHidesCancel   bool // commits when the source's last read returns EOF after ctx ended
}

type object struct {
	data []byte
	info storage.ObjectInfo
}

func newFake() *fake { return &fake{objects: map[string]object{}} }

func (f *fake) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if err := f.checkKey(key); err != nil {
		return storage.ObjectInfo{}, f.wrap("put", key, err)
	}
	if !f.noOptionsCheck {
		if err := storage.ValidatePutOptions(opts); err != nil {
			return storage.ObjectInfo{}, f.wrap("put", key, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, f.wrap("put", key, err)
	}
	if !f.ignoresSize && opts.Size != nil {
		r = storage.ExpectSize(r, *opts.Size)
	}
	info := storage.ObjectInfo{Key: key, ContentType: opts.ContentType, ModTime: time.Now()}
	if f.baseNameKey {
		info.Key = path.Base(key)
	}
	if f.keepsOldType {
		if old, err := f.get("put", key); err == nil {
			info.ContentType = old.info.ContentType
		}
	}
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
	_, existed := f.lookup(key)
	for {
		if !f.ignoresCtx {
			if err := ctx.Err(); err != nil {
				return storage.ObjectInfo{}, f.wrap("put", key, err)
			}
		}
		n, err := r.Read(chunk)
		buf.Write(chunk[:n])
		if f.nonAtomic || (f.nonAtomicFirst && !existed) {
			f.store(key, buf.Bytes(), info)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return storage.ObjectInfo{}, f.wrap("put", key, err)
		}
	}
	// The source's last read may have returned (EOF) after ctx ended:
	// cancellation wins, and nothing is committed.
	if !f.ignoresCtx && !f.eofHidesCancel {
		if err := ctx.Err(); err != nil {
			return storage.ObjectInfo{}, f.wrap("put", key, err)
		}
	}
	data := buf.Bytes()
	if f.truncates && len(data) > 1<<20 {
		data = data[:1<<20]
	}
	if f.rejectsEmpty && len(data) == 0 {
		return storage.ObjectInfo{}, f.wrap("put", key, fmt.Errorf("%w: empty object", storage.ErrInvalidOptions))
	}
	if f.tornWrites {
		f.writeTorn(key, data, info)
		info.Size = int64(len(data))
		return info, nil
	}
	info.Size = int64(len(data))
	sum := sha256.Sum256(data)
	info.ETag = hex.EncodeToString(sum[:])
	if f.constantETag {
		info.ETag = "constant"
	}
	f.store(key, data, info)
	if f.aliasMetadata {
		o, _ := f.lookup(key)
		return o.info, nil
	}
	if f.resultIsInput {
		info.Metadata = opts.Metadata
	}
	if f.putNoETag {
		info.ETag = ""
	}
	return info, nil
}

// writeTorn overwrites key's bytes in place, only every other 32 KiB
// chunk: what an unsynchronized in-place writer can leave behind.
func (f *fake) writeTorn(key string, data []byte, info storage.ObjectInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[f.mapKey(key)]
	if len(o.data) < len(data) {
		o.data = append(o.data, make([]byte, len(data)-len(o.data))...)
	}
	for off := 0; off < len(data); off += 64 << 10 {
		end := min(off+32<<10, len(data))
		copy(o.data[off:end], data[off:end])
	}
	info.Size = int64(len(o.data))
	o.info = info
	f.objects[f.mapKey(key)] = o
}

// wrap is storage.Wrap, unless the fake returns bare sentinels.
func (f *fake) wrap(op, key string, err error) error {
	if f.bareErrors {
		return err
	}
	var se *storage.Error
	if f.keepsForeignErr && errors.As(err, &se) {
		return err
	}
	return storage.Wrap(op, key, err)
}

// owned returns info with a metadata map of its own.
func (f *fake) owned(info storage.ObjectInfo) storage.ObjectInfo {
	if f.aliasMetadata || info.Metadata == nil {
		return info
	}
	md := make(map[string]string, len(info.Metadata))
	for k, v := range info.Metadata {
		md[k] = v
	}
	info.Metadata = md
	return info
}

func (f *fake) mapKey(key string) string {
	if f.caseInsensitive {
		return strings.ToLower(key)
	}
	return key
}

func (f *fake) readCtx(ctx context.Context) error {
	if f.readIgnoresCtx {
		return nil
	}
	return ctx.Err()
}

func (f *fake) store(key string, data []byte, info storage.ObjectInfo) {
	info.Size = int64(len(data))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[f.mapKey(key)] = object{data: append([]byte(nil), data...), info: f.owned(info)}
}

// lookup reports whether key holds an object.
func (f *fake) lookup(key string) (object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[f.mapKey(key)]
	return o, ok
}

func (f *fake) checkKey(key string) error {
	if f.noKeyCheck {
		return nil
	}
	return storage.ValidateKey(key)
}

func (f *fake) get(op, key string) (object, error) {
	if err := f.checkKey(key); err != nil {
		return object{}, f.wrap(op, key, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[f.mapKey(key)]
	if !ok {
		return object{}, f.wrap(op, key, storage.ErrNotFound)
	}
	o.info = f.owned(o.info)
	return o, nil
}

func (f *fake) Open(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	if err := f.checkKey(key); err != nil {
		return nil, storage.ObjectInfo{}, f.wrap("open", key, err)
	}
	if err := f.readCtx(ctx); err != nil {
		return nil, storage.ObjectInfo{}, f.wrap("open", key, err)
	}
	o, err := f.get("open", key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	body := io.NopCloser(bytes.NewReader(o.data))
	if f.openNoETag {
		o.info.ETag = ""
	}
	if f.openDetached {
		return body, o.info, nil
	}
	return storage.ContextReadCloser(ctx, body), o.info, nil
}

func (f *fake) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := f.checkKey(key); err != nil {
		return storage.ObjectInfo{}, f.wrap("stat", key, err)
	}
	if err := f.readCtx(ctx); err != nil {
		return storage.ObjectInfo{}, f.wrap("stat", key, err)
	}
	o, err := f.get("stat", key)
	return o.info, err
}

func (f *fake) Delete(ctx context.Context, key string) error {
	if err := f.checkKey(key); err != nil {
		return f.wrap("delete", key, err)
	}
	if err := f.readCtx(ctx); err != nil {
		return f.wrap("delete", key, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[f.mapKey(key)]; !ok && f.deleteMissingErr {
		return f.wrap("delete", key, storage.ErrNotFound)
	}
	delete(f.objects, f.mapKey(key))
	if f.deleteByPrefix {
		for k := range f.objects {
			if strings.HasPrefix(k, path.Dir(key)+"/") {
				delete(f.objects, k)
			}
		}
	}
	return nil
}

func (f *fake) URL(ctx context.Context, key string, opts storage.URLOptions) (string, error) {
	if err := f.checkKey(key); err != nil {
		return "", f.wrap("url", key, err)
	}
	if err := f.readCtx(ctx); err != nil {
		return "", f.wrap("url", key, err)
	}
	if err := storage.ValidateURLOptions(opts); err != nil && (!f.anyExpiry || opts.Expires <= storage.MaxURLExpiry) {
		return "", f.wrap("url", key, err)
	}
	if f.signedNotPublic {
		return "", f.wrap("url", key, storage.ErrNotPublic)
	}
	if f.urlNeedsObject {
		if _, err := f.get("url", key); err != nil {
			return "", err
		}
	}
	if f.emptyURL {
		return "", nil
	}
	return "", f.wrap("url", key, storage.ErrUnsupported)
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
		{"InvalidKeys", func(f *fake) { f.noKeyCheck = true }, "want storage: invalid object key"},
		{"SizeMismatch", func(f *fake) { f.ignoresSize = true }, "want storage: object length differs"},
		{"CanceledPut", func(f *fake) { f.ignoresCtx = true }, "want context canceled"},
		{"Missing", func(f *fake) { f.deleteMissingErr = true }, "deletes are idempotent"},
		{"DefaultContentType", func(f *fake) { f.noDefaultType = true }, "want \"application/octet-stream\""},
		{"URL", func(f *fake) { f.emptyURL = true }, "empty URL and no error"},
		{"RoundTrip", func(f *fake) { f.dropsMetadata = true }, "metadata"},
		{"Overwrite", func(f *fake) { f.keepsOldType = true }, "ContentType"},
		{"Delete", func(f *fake) { f.deleteByPrefix = true }, "a/c"},
		{"NestedKeys", func(f *fake) { f.baseNameKey = true }, "with the same key"},
		{"Streaming", func(f *fake) { f.truncates = true }, "want 8388608"},
		{"EmptyObject", func(f *fake) { f.rejectsEmpty = true }, "Put(\"empty\")"},
		{"InvalidOptions", func(f *fake) { f.noOptionsCheck = true }, "want storage.ErrInvalidOptions"},
		{"ConcurrentPuts", func(f *fake) { f.tornWrites = true }, "torn write"},
		{"CanceledContext", func(f *fake) { f.readIgnoresCtx = true }, "want context canceled"},
		{"PortableKeys", func(f *fake) { f.caseInsensitive = true }, "two keys share one object"},
		{"URL", func(f *fake) { f.urlNeedsObject = true }, "must not check that the object exists"},
		{"NoPartialReads", func(f *fake) { f.nonAtomic = true }, "part of the object being written"},
		{"InvalidOptions", func(f *fake) { f.anyExpiry = true }, "MaxURLExpiry"},
		{"URL", func(f *fake) { f.signedNotPublic = true }, "a signed URL works for a private object"},
		{"NoPartialReads", func(f *fake) { f.nonAtomicFirst = true }, "mid-Put of a new key"},
		{"MetadataIsOwned", func(f *fake) { f.aliasMetadata = true }, "the stored metadata changed without a Put"},
		{"MetadataIsOwned", func(f *fake) { f.resultIsInput = true }, "shares the caller's map"},
		{"Overwrite", func(f *fake) { f.constantETag = true }, "different bytes kept the ETag"},
		{"Overwrite", func(f *fake) { f.putNoETag = true }, "they must agree"},
		{"Overwrite", func(f *fake) { f.openNoETag = true }, "they must agree"},
		{"FailedPutKeepsPrevious", func(f *fake) { f.keepsForeignErr = true }, "inside a *storage.Error"},
		{"CanceledPut", func(f *fake) { f.eofHidesCancel = true }, "want context canceled"},
		{"OpenFollowsContext", func(f *fake) { f.openDetached = true }, "must follow the context"},
		{"Missing", func(f *fake) { f.bareErrors = true }, "inside a *storage.Error"},
		{"InvalidKeys", func(f *fake) { f.bareErrors = true }, "inside a *storage.Error"},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.check] = true
	}
	for _, c := range checks {
		if !covered[c.name] {
			t.Errorf("check %s has no broken driver proving it can fail", c.name)
		}
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
