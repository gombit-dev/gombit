// Package atomicfile replaces a file's content so that it is never seen
// truncated or half-written.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes content to a temp file in the target's directory, fsyncs and
// closes it, sets mode, then renames it over path. The rename is atomic on the
// same filesystem, so path is never observed truncated; a failure anywhere
// before the rename leaves path untouched and removes the temp file.
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
	return os.Rename(tmpName, path)
}
