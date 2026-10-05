// Package atomicfile replaces a file's content so that it is never seen
// truncated or half-written: a reader finds the old content or the new,
// whole. That is the guarantee on every platform (atomic visibility).
// Surviving a crash (durability) is promised only where the code below asks
// the filesystem for it: on Unix, not on Windows.
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
// FILE_RENAME_REPLACE_IF_EXISTS, NTFS on Windows 10 1709 or later), the same
// primitive storage/local's writes rest on. A volume without it (FAT, exFAT,
// an older Windows) falls back to MoveFileEx(MOVEFILE_REPLACE_EXISTING |
// MOVEFILE_WRITE_THROUGH), which Windows does not document as atomic: there
// the old-or-new guarantee is the filesystem's, not this package's. Nothing
// here makes the POSIX-semantics rename durable before Write returns, so on
// Windows a power loss right after a successful Write may still leave the
// old content.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes content, with mode, to a temp file in the target's directory,
// fsyncs and closes it, renames it over path, and (on Unix) fsyncs the
// directory: see the package doc for what that rename guarantees on each
// platform. A failure anywhere before the rename, or a refused rename, leaves
// path untouched and removes the temp file. A failed directory sync is
// returned too: path then holds the new content, which may not survive a
// crash.
func Write(path string, content []byte, mode os.FileMode) error {
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
	if err := replace(tmpName, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
