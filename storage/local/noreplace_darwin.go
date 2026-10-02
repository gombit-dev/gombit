package local

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames src to dst unless dst exists (fs.ErrExist), in
// one atomic step (renamex_np RENAME_EXCL), falling back to a hard link on
// a filesystem without it.
func renameNoReplace(src, dst string) error {
	err := unix.RenamexNp(src, dst, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return linkNoReplace(src, dst)
	}
	if err != nil {
		return &renameError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
