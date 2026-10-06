package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/gombit-dev/gombit/internal/winfile"
)

// replace renames src over dst with POSIX semantics (winfile.RenameByHandle),
// which replace dst in one step. A volume without them refuses that rename
// with dst unchanged. replace then refuses too (ErrNoAtomicRename), unless
// the caller allowed a non-atomic replace: os.Rename, which Windows does not
// document as atomic.
func replace(src, dst string, o options) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	err = renameHook(absSrc, absDst, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
	switch {
	case err == nil || !winfile.PosixUnsupported(err):
		return err
	case o.allowNonAtomic:
		return os.Rename(src, dst)
	default:
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: fmt.Errorf("%w (%w)", ErrNoAtomicRename, err)}
	}
}

// renameHook is renameByHandle; a test replaces it to play a volume
// without POSIX rename semantics.
var renameHook = renameByHandle

// renameByHandle renames the absolute path src to dst with flags, on a
// handle opened for it with every share mode, so that a reader holding dst
// open with delete sharing does not block the rename.
func renameByHandle(src, dst string, flags uint32) error {
	p, err := windows.UTF16PtrFromString(winfile.LongPath(src))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	err = winfile.RenameByHandle(h, dst, flags)
	_ = windows.CloseHandle(h)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

// syncDir does nothing on Windows: none of the calls replace uses makes the
// rename durable before it returns (see the package doc).
func syncDir(string) error { return nil }
