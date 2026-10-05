// Package atomicfile replaces a file's content so that it is never seen
// truncated or half-written: a reader, or a crash, finds the old content or
// the new, whole.
//
// The new content goes to a temp file in the target's directory, which is
// then renamed over the target. A rename is atomic where the platform says
// so: on Unix, rename(2) replaces the target in one step. On Windows, Go's
// os.Rename makes no such promise, so the rename there is the one Windows
// gives POSIX semantics (SetFileInformationByHandle, FileRenameInfoEx with
// FILE_RENAME_POSIX_SEMANTICS and FILE_RENAME_REPLACE_IF_EXISTS, NTFS on
// Windows 10 1709 or later), the same primitive storage/local's writes rest
// on. A volume without it (FAT, exFAT, an older Windows) falls back to
// MoveFileEx(MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH), which
// Windows does not document as atomic: there the old-or-new guarantee is
// the filesystem's, not this package's.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes content to a temp file in the target's directory, fsyncs and
// closes it, sets mode, then renames it over path (see the package doc for
// what makes that rename atomic). A failure anywhere before the rename, or
// a refused rename, leaves path untouched and removes the temp file.
func Write(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gombit-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// If we return before the rename, the temp file must not linger. After a
	// successful rename tmpName no longer exists and this is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()
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
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return replace(tmpName, path)
}
