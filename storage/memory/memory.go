// Package memory is an in-process storage.Storage for tests: objects live in
// a map, nothing touches the filesystem, and every behavior is
// deterministic. It keeps the whole storage contract (it passes
// storagetest), but it buffers each object in memory, so it is not for
// large files or for production.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/gombit-dev/gombit/storage"
)

// Store is an in-memory storage.Storage. The zero value is not usable; call
// New. It is safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	objects map[string]object
	now     func() time.Time
}

type object struct {
	data []byte // never modified once stored: a Put replaces the slice
	info storage.ObjectInfo
}

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock that stamps ObjectInfo.ModTime (default
// time.Now), for tests that assert on it.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New returns an empty in-memory store.
func New(opts ...Option) *Store {
	s := &Store{objects: map[string]object{}, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ storage.Storage = (*Store)(nil)

// Put implements storage.Storage. The object is read completely before it
// replaces the old one, so a failed Put changes nothing.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	if err := storage.ValidatePutOptions(opts); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, storage.ContextReader(ctx, storage.ExpectSize(r, opts.Size))); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, storage.Wrap("put", key, err)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	info := storage.ObjectInfo{
		Key:         key,
		Size:        int64(len(data)),
		ContentType: opts.ContentType,
		ETag:        hex.EncodeToString(sum[:]),
		ModTime:     s.now(),
		Metadata:    maps.Clone(opts.Metadata),
	}
	if info.ContentType == "" {
		info.ContentType = storage.DefaultContentType
	}
	if len(info.Metadata) == 0 {
		info.Metadata = nil
	}
	s.mu.Lock()
	s.objects[key] = object{data: data, info: info}
	s.mu.Unlock()
	return copyInfo(info), nil
}

func (s *Store) get(ctx context.Context, op, key string) (object, error) {
	if err := storage.ValidateKey(key); err != nil {
		return object{}, storage.Wrap(op, key, err)
	}
	if err := ctx.Err(); err != nil {
		return object{}, storage.Wrap(op, key, err)
	}
	s.mu.RLock()
	o, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return object{}, storage.Wrap(op, key, storage.ErrNotFound)
	}
	return o, nil
}

// Open implements storage.Storage.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	o, err := s.get(ctx, "open", key)
	if err != nil {
		return nil, storage.ObjectInfo{}, err
	}
	return io.NopCloser(bytes.NewReader(o.data)), copyInfo(o.info), nil
}

// Stat implements storage.Storage.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	o, err := s.get(ctx, "stat", key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return copyInfo(o.info), nil
}

// Delete implements storage.Storage.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return storage.Wrap("delete", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.Wrap("delete", key, err)
	}
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

// URL implements storage.Storage. Objects in memory have no URL: it returns
// storage.ErrUnsupported (after validating its arguments).
func (s *Store) URL(ctx context.Context, key string, opts storage.URLOptions) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := storage.ValidateURLOptions(opts); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	if err := ctx.Err(); err != nil {
		return "", storage.Wrap("url", key, err)
	}
	return "", storage.Wrap("url", key, storage.ErrUnsupported)
}

// Keys returns the stored keys, for tests that assert what was written.
// The order is unspecified.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	return keys
}

// copyInfo returns info with its own Metadata map, so a caller cannot
// change what is stored.
func copyInfo(info storage.ObjectInfo) storage.ObjectInfo {
	info.Metadata = maps.Clone(info.Metadata)
	return info
}
