package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// replace renames src over dst with POSIX semantics, which replace dst in
// one step; where the volume or Windows version has none, with MoveFileEx
// (see the package doc).
func replace(src, dst string) error {
	abs, err := filepath.Abs(dst)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	err = renameByHandle(src, abs, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
	if err == nil || !posixUnsupported(err) {
		return err
	}
	from, ferr := windows.UTF16PtrFromString(longPath(src))
	to, terr := windows.UTF16PtrFromString(longPath(abs))
	if ferr != nil || terr != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errors.Join(ferr, terr)}
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

// syncDir does nothing on Windows: none of the calls replace uses makes the
// rename durable before it returns (see the package doc).
func syncDir(string) error { return nil }

// posixUnsupported reports a filesystem (FAT, say) or Windows version
// without POSIX rename semantics.
func posixUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

// fileRenameInfo is FILE_RENAME_INFO with its Flags member.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// renameByHandle renames src to the absolute path dst with
// SetFileInformationByHandle (FileRenameInfoEx) and flags.
func renameByHandle(src, dst string, flags uint32) error {
	name, err := windows.UTF16FromString(longPath(dst))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	p, err := windows.UTF16PtrFromString(longPath(src))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info fileRenameInfo
	nameBytes := (len(name) - 1) * 2 // without the terminating NUL
	buf := make([]byte, int(unsafe.Offsetof(info.FileName))+len(name)*2)
	ri := (*fileRenameInfo)(unsafe.Pointer(&buf[0])) // #nosec G103 -- FILE_RENAME_INFO over a buffer sized for its name
	ri.Flags = flags
	ri.FileNameLength = uint32(nameBytes)                                                                              // #nosec G115 -- a path, far below 4 GiB
	copy(unsafe.Slice(&ri.FileName[0], len(name)), name)                                                               // #nosec G103 -- within buf, as above
	if err := windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(len(buf))); err != nil { // #nosec G115 -- as above
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

// longPath returns an absolute path in the form raw Win32 calls need to
// reach it when it is long (248 characters or more) on Windows versions
// that do not opt the process into long paths: the \\?\ prefix, or
// \\?\UNC\ for a share. It is what os.fixLongPath does for os's own calls.
func longPath(path string) string {
	if len(path) < 248 || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	path = filepath.Clean(path)
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
