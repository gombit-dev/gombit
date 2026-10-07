// Package atomicfile replaces a file's content so that it is never seen
// truncated or half-written: a reader finds the old content or the new,
// whole (atomic visibility). Write gives that guarantee or does nothing:
// where the platform has no atomic rename for the target's volume, it
// refuses (ErrNoAtomicRename) before touching the target, unless the caller
// passed AllowNonAtomic, which accepts a replace without the guarantee on
// such a volume. Surviving a crash (durability) is promised only where the
// code below asks the filesystem for it: on Unix, not on Windows.
//
// The new content, with its mode, goes to a temp file in the target's
// directory and is synced; the temp file is then renamed over the target.
//
// On Unix the rename is rename(2), which replaces the target in one step,
// and Write then fsyncs the directory, which fsync(2) requires for the
// rename itself to reach stable storage. Once Write returns nil, a crash
// leaves the new content, on a filesystem that honors fsync of a directory
// (a filesystem that cannot sync a directory, where fsync reports EINVAL,
// gets visibility only).
//
// On Windows, Go's os.Rename makes no atomicity promise, so the rename is the
// one Windows gives POSIX semantics (SetFileInformationByHandle,
// FileRenameInfoEx with FILE_RENAME_POSIX_SEMANTICS and
// FILE_RENAME_REPLACE_IF_EXISTS: internal/winfile, which storage/local's
// writes use too). A volume that refuses it (FAT and exFAT, Windows before 10
// 1709, and redirector volumes: network shares, mapped drives, \\wsl$ paths)
// refuses without changing anything, and Write then fails with
// ErrNoAtomicRename: no rename Windows offers there (MoveFileEx included) is
// documented as atomic. With AllowNonAtomic, Write uses os.Rename there
// instead, as Gombit did before this package existed. Nothing
// here makes the POSIX-semantics rename durable before Write returns, so on
// Windows a power loss right after a successful Write may still leave the
// old content.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoAtomicRename: the target's volume refused an atomic
// (POSIX-semantics) rename, so Write refused before changing the target. It
// wraps errors.ErrUnsupported. Only Windows returns it (see the package doc
// for the volumes that do this).
var ErrNoAtomicRename = fmt.Errorf("atomicfile: the volume refused an atomic rename (as FAT and exFAT volumes, network shares, \\\\wsl$ paths and Windows before 10 1709 do), so the file was not replaced: %w", errors.ErrUnsupported)

// Option adjusts Write.
type Option func(*options)

type options struct{ allowNonAtomic bool }

// AllowNonAtomic lets Write replace the file without the atomic guarantee
// on a volume that refuses an atomic rename (Windows only; see the package
// doc), instead of failing with ErrNoAtomicRename. For a caller that can
// undo a half-done replace another way, such as a transaction with its own
// rollback.
func AllowNonAtomic() Option { return func(o *options) { o.allowNonAtomic = true } }

// ErrNotDurable: Write put the new content in place (readers see it), but
// the directory sync that makes the replacement survive a crash failed. A
// Write error wrapping it is a post-commit error; see Committed.
var ErrNotDurable = errors.New("atomicfile: the file was replaced, but the replacement was not synced to disk")

// Committed reports whether, after Write returned err, the target holds the
// new content: on success, and on an ErrNotDurable failure. Any other error
// left the target untouched. A caller that undoes its writes must treat a
// committed write as done, whatever the error.
func Committed(err error) bool {
	return err == nil || errors.Is(err, ErrNotDurable)
}

// syncDirHook is the directory sync Write runs after the rename;
// SetSyncDirForTest replaces it.
var syncDirHook = syncDir

// SetSyncDirForTest makes Write sync directories with f until the returned
// restore is called, so that callers can test their handling of a
// post-commit failure (ErrNotDurable). Not for concurrent use with Write.
func SetSyncDirForTest(f func(dir string) error) (restore func()) {
	prev := syncDirHook
	syncDirHook = f
	return func() { syncDirHook = prev }
}

// Write writes content, with mode, to a temp file in the target's directory,
// fsyncs and closes it, renames it over path, and (on Unix) fsyncs the
// directory: see the package doc for what that rename guarantees on each
// platform. A failure anywhere before the rename, or a refused rename, leaves
// path untouched and removes the temp file. A failed directory sync comes
// after the commit: the error wraps ErrNotDurable, and path holds the new
// content, which may not survive a crash (Committed reports true).
func Write(path string, content []byte, mode os.FileMode, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gombit-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// If we return before the rename, the temp file must not linger. After a
	// successful rename tmpName no longer exists and this is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()
	// The mode is set before the sync, so the sync persists it with the
	// content.
	if err := os.Chmod(tmpName, mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replace(tmpName, path, o); err != nil {
		return err
	}
	if err := syncDirHook(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: sync %s: %w", ErrNotDurable, filepath.Dir(path), err)
	}
	return nil
}
