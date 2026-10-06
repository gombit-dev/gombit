package winfile

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileRenameInfo is FILE_RENAME_INFO with its Flags member.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// RenameByHandle renames the file open as h (opened with DELETE access) to
// the absolute path dst, with SetFileInformationByHandle (FileRenameInfoEx)
// and flags: windows.FILE_RENAME_POSIX_SEMANTICS, and
// windows.FILE_RENAME_REPLACE_IF_EXISTS to replace dst. A volume or Windows
// version without POSIX rename semantics refuses it with dst unchanged;
// PosixUnsupported recognizes that error.
func RenameByHandle(h windows.Handle, dst string, flags uint32) error {
	name, err := windows.UTF16FromString(LongPath(dst))
	if err != nil {
		return err
	}
	var info fileRenameInfo
	nameBytes := (len(name) - 1) * 2 // without the terminating NUL
	buf := make([]byte, int(unsafe.Offsetof(info.FileName))+len(name)*2)
	ri := (*fileRenameInfo)(unsafe.Pointer(&buf[0])) // #nosec G103 -- FILE_RENAME_INFO over a buffer sized for its name
	ri.Flags = flags
	ri.FileNameLength = uint32(nameBytes)                                                             // #nosec G115 -- a path, far below 4 GiB
	copy(unsafe.Slice(&ri.FileName[0], len(name)), name)                                              // #nosec G103 -- within buf, as above
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(len(buf))) // #nosec G115 -- as above
}

// PosixUnsupported reports the error of a volume, or Windows version,
// that refuses a POSIX-semantics rename (or delete): FAT and exFAT, Windows
// before 10 1709, and redirector volumes such as network shares and
// \\wsl$ paths, which answer ERROR_INVALID_PARAMETER.
func PosixUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}

// LongPath returns path in the form raw Win32 calls need to reach it when
// it is long. The os package does the same for its own calls
// (os.fixLongPath): on Windows 10 1703 and later the Go runtime opts the
// process into long paths and neither is needed, but on the older versions
// Go supports (Windows Server 2016, say) a path of 248 characters or more
// works only with the extended \\?\ prefix, or \\?\UNC\ for a share. The
// path must be absolute and clean, as the prefix requires.
func LongPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
