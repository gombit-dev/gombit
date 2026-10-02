package local

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames src to dst unless dst exists (fs.ErrExist), in
// one atomic step (renameat2 RENAME_NOREPLACE), falling back to a hard link
// on a filesystem without it.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return linkNoReplace(src, dst)
	}
	if err != nil {
		return &renameError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
