// Package local is a storage.Storage on the local filesystem, for
// development and single-host deployments: objects are files under one root
// directory, streamed to and from disk.
//
// # Layout
//
// A key is not used as a path. Each object is stored at
//
//	<root>/objects/<h[0:2]>/<h[2:4]>/<h>
//
// where h is the hex SHA-256 of the key. Joining the key to the root would
// break the storage contract on real filesystems: keys that differ only in
// case collide on macOS and Windows, "a" and "a/b" cannot both exist (a
// file cannot also be a directory), a segment may exceed the filesystem's
// name limit, and Windows reserves names such as "CON". Hashing makes every
// valid key its own file, and no key can reach outside the root.
//
// A file holds the object's bytes followed by a trailer: a JSON header (a
// format version, the key, content type, metadata, modification time, and a
// SHA-256 ETag), its length, and a magic number. Open serves the bytes
// before the trailer. A reader ignores header fields it does not know, so a
// later version may add fields and still share a root with this one; the
// version changes only for a change older readers cannot read.
//
// # Writes
//
// Put streams into a temporary file in <root>/tmp, never holding the object
// in memory, flushes it to disk, renames it into place, and flushes the
// directory: the rename is atomic, so a reader sees the old object or the
// new one, never part of one, and a Put that returned is on disk. A failed
// Put removes its temporary file. A process killed mid-Put cannot, so a
// store's first Put removes temporary files nothing has written to for an
// hour. The root and its directories are created on the first Put.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gombit-dev/gombit/storage"
)

// Store is a storage.Storage under a root directory. It is safe for
// concurrent use, including by several processes sharing the root.
type Store struct {
	root     string
	now      func() time.Time
	firstPut func(root string)
	warn     func(msg string, err error)
	once     sync.Once
}

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock that stamps ObjectInfo.ModTime (default
// time.Now), for tests that assert on it.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithFirstPut sets a function called with the root on the store's first
// Put (framework.New uses it to warn about an ephemeral root in
// production).
func WithFirstPut(fn func(root string)) Option {
	return func(s *Store) { s.firstPut = fn }
}

// WithWarn sets a function told about failures that do not fail the
// operation, such as a stored object whose directory entry could not be
// flushed to disk (framework.New sends these to the app's logger).
func WithWarn(fn func(msg string, err error)) Option {
	return func(s *Store) { s.warn = fn }
}

// staleTemp is how long a temporary file may go unwritten before a store
// treats it as abandoned by a process that died mid-Put. A live Put writes
// continuously, so its file's modification time stays current.
const staleTemp = time.Hour

// New returns a store under root. root is made absolute, so the store does
// not follow a later change of working directory. Nothing is created until
// the first Put.
func New(root string, opts ...Option) (*Store, error) {
	if root == "" {
		return nil, errors.New("local storage: empty root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("local storage: root %q: %w", root, err)
	}
	s := &Store{root: abs, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

var _ storage.Storage = (*Store)(nil)

// Root returns the absolute root directory.
func (s *Store) Root() string { return s.root }

// path is where key's object lives: under the root whatever the key is.
func (s *Store) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(s.root, "objects", h[0:2], h[2:4], h)
}

const (
	// magic ends every object file.
	magic = "GOMBITOB"
	// trailerTail is the length field plus the magic.
	trailerTail = 4 + len(magic)
	// maxHeader bounds a header, so a corrupt length cannot ask for an
	// unbounded read.
	maxHeader = 64 << 10
	// formatVersion is the trailer layout this package writes and reads.
	formatVersion = 1
)

// header is the object's description, stored after its bytes.
type header struct {
	Version     int               `json:"v"`
	Key         string            `json:"key"`
	ContentType string            `json:"content_type"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ModTime     time.Time         `json:"mod_time"`
	ETag        string            `json:"etag"`
}

// Put implements storage.Storage.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	info, err := s.put(ctx, key, r, opts)
	return info, storage.Wrap("put", key, err)
}

func (s *Store) put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := storage.ValidatePutOptions(opts); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	s.once.Do(func() {
		s.sweepTemp()
		if s.firstPut != nil {
			s.firstPut(s.root)
		}
	})
	tmpDir := filepath.Join(s.root, "tmp")
	tmp, err := os.CreateTemp(tmpDir, "put-*")
	if errors.Is(err, fs.ErrNotExist) {
		// The first Put: create the root and flush its entry (in its parent)
		// and tmp's (in the root) now, before any object is published, so a
		// crash cannot drop the root under objects renamed into it.
		if err = os.MkdirAll(tmpDir, 0o750); err == nil {
			if err = syncDir(filepath.Dir(s.root)); err == nil {
				err = syncDir(s.root)
			}
		}
		if err == nil {
			tmp, err = os.CreateTemp(tmpDir, "put-*")
		}
	}
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(tmp, hash), storage.PutReader(ctx, r, opts), make([]byte, 32<<10))
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	h := header{
		Version:     formatVersion,
		Key:         key,
		ContentType: opts.ContentType,
		Metadata:    maps.Clone(opts.Metadata),
		ModTime:     s.now().UTC(),
		ETag:        hex.EncodeToString(hash.Sum(nil)),
	}
	if h.ContentType == "" {
		h.ContentType = storage.DefaultContentType
	}
	if len(h.Metadata) == 0 {
		h.Metadata = nil
	}
	if err := writeTrailer(tmp, h); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := tmp.Sync(); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err := tmp.Close(); err != nil {
		return storage.ObjectInfo{}, err
	}
	// The last point a cancellation can still leave the key untouched.
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	dst := s.path(key)
	err = os.Rename(tmp.Name(), dst)
	if errors.Is(err, fs.ErrNotExist) {
		// The fan-out directories do not exist yet: create them, and flush
		// each new entry up to the root, so the object survives a crash.
		if err = os.MkdirAll(filepath.Dir(dst), 0o750); err == nil {
			for dir := filepath.Dir(dst); dir != s.root && err == nil; dir = filepath.Dir(dir) {
				err = syncDir(filepath.Dir(dir))
			}
			if err == nil {
				err = os.Rename(tmp.Name(), dst)
			}
		}
	}
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	// The rename published the object: from here the Put has happened, and
	// returning an error would tell the caller it did not. Flush the
	// directory entry so it survives a crash; if that fails even on a
	// retry, report it through the warning hook and return success.
	committed = true
	if err := flushRenameHook(dst); err != nil {
		if err = flushRenameHook(dst); err != nil && s.warn != nil {
			s.warn("local storage: an object was stored, but flushing its directory entry to disk failed; it may not survive a crash", err)
		}
	}
	return h.info(n), nil
}

// flushRenameHook is flushRename, replaceable by tests.
var flushRenameHook = flushRename

// flushRename makes a rename to dst durable. On Unix that is an fsync of
// the directory that holds the new entry. Windows cannot open a directory
// to flush it; there, flushing the renamed file itself (FlushFileBuffers)
// writes its metadata, which NTFS journals together with the rename.
func flushRename(dst string) error {
	if runtime.GOOS != "windows" {
		return syncDir(filepath.Dir(dst))
	}
	f, err := os.OpenFile(dst, os.O_RDWR, 0) // #nosec G304 -- an object file under the store's own root
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncDir flushes a directory's entries (a rename or a new subdirectory) to
// disk. Windows cannot open a directory to flush it, so it is a no-op there;
// flushRename covers a rename on Windows by flushing the renamed file.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir) // #nosec G304 -- a directory under the store's own root
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// sweepTemp removes temporary files that nothing has written to for
// staleTemp: what a process killed mid-Put left behind. Errors are
// ignored; a file another process just removed is fine.
func (s *Store) sweepTemp() {
	tmpDir := filepath.Join(s.root, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleTemp)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "put-") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(tmpDir, e.Name()))
		}
	}
}

func writeTrailer(w io.Writer, h header) error {
	js, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(js) > maxHeader {
		return fmt.Errorf("%w: object description is %d bytes, more than %d", storage.ErrInvalidOptions, len(js), maxHeader)
	}
	var tail [trailerTail]byte
	binary.BigEndian.PutUint32(tail[:4], uint32(len(js))) // #nosec G115 -- bounded by maxHeader above
	copy(tail[4:], magic)
	_, err = w.Write(append(js, tail[:]...))
	return err
}

func (h header) info(size int64) storage.ObjectInfo {
	return storage.ObjectInfo{
		Key:         h.Key,
		Size:        size,
		ContentType: h.ContentType,
		ETag:        h.ETag,
		ModTime:     h.ModTime,
		Metadata:    maps.Clone(h.Metadata),
	}
}

// open opens key's file and reads its trailer, returning the file, the
// object's length, and its header. The caller closes the file.
func (s *Store) open(ctx context.Context, key string) (*os.File, int64, header, error) {
	if err := storage.ValidateKey(key); err != nil {
		return nil, 0, header{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, header{}, err
	}
	f, err := os.Open(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, header{}, storage.ErrNotFound
	}
	if err != nil {
		return nil, 0, header{}, err
	}
	size, h, err := readTrailer(f)
	if err == nil && h.Key != key {
		err = fmt.Errorf("%s holds key %q, not %q", f.Name(), h.Key, key)
	}
	if err != nil {
		_ = f.Close()
		return nil, 0, header{}, err
	}
	return f, size, h, nil
}

// errCorrupt reports an object file whose trailer cannot be read: a write
// outside this package, or a damaged disk.
var errCorrupt = errors.New("local storage: corrupt object file")

func readTrailer(f *os.File) (int64, header, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, header{}, err
	}
	total := fi.Size()
	if total < int64(trailerTail) {
		return 0, header{}, fmt.Errorf("%w: %s is %d bytes", errCorrupt, f.Name(), total)
	}
	var tail [trailerTail]byte
	if _, err := f.ReadAt(tail[:], total-int64(trailerTail)); err != nil {
		return 0, header{}, err
	}
	if string(tail[4:]) != magic {
		return 0, header{}, fmt.Errorf("%w: %s has no trailer", errCorrupt, f.Name())
	}
	hlen := int64(binary.BigEndian.Uint32(tail[:4]))
	if hlen > maxHeader || hlen > total-int64(trailerTail) {
		return 0, header{}, fmt.Errorf("%w: %s declares a %d-byte header", errCorrupt, f.Name(), hlen)
	}
	js := make([]byte, hlen)
	start := total - int64(trailerTail) - hlen
	if _, err := f.ReadAt(js, start); err != nil {
		return 0, header{}, err
	}
	var h header
	if err := json.Unmarshal(js, &h); err != nil {
		return 0, header{}, fmt.Errorf("%w: %s: %v", errCorrupt, f.Name(), err)
	}
	if h.Version != formatVersion {
		return 0, header{}, fmt.Errorf("local storage: %s uses object format v%d; this version reads v%d", f.Name(), h.Version, formatVersion)
	}
	return start, h, nil
}

// Open implements storage.Storage. The object is read from disk as the
// caller reads it.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, storage.ObjectInfo, error) {
	f, size, h, err := s.open(ctx, key)
	if err != nil {
		return nil, storage.ObjectInfo{}, storage.Wrap("open", key, err)
	}
	return &objectReader{SectionReader: io.NewSectionReader(f, 0, size), f: f}, h.info(size), nil
}

type objectReader struct {
	*io.SectionReader
	f *os.File
}

func (r *objectReader) Close() error { return r.f.Close() }

// Stat implements storage.Storage.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	f, size, h, err := s.open(ctx, key)
	if err != nil {
		return storage.ObjectInfo{}, storage.Wrap("stat", key, err)
	}
	_ = f.Close()
	return h.info(size), nil
}

// Delete implements storage.Storage. Emptied directories are left in place.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return storage.Wrap("delete", key, err)
	}
	if err := ctx.Err(); err != nil {
		return storage.Wrap("delete", key, err)
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return storage.Wrap("delete", key, err)
	}
	return nil
}

// URL implements storage.Storage. Files on local disk have no URL until
// visibility and signed URLs land (STORAGE-5); it returns
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
