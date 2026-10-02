package local

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows an open file locks its name unless every handle on it was
// opened with FILE_SHARE_DELETE, and even then a plain rename or delete of
// it fails or leaves the name taken until the last handle closes. The
// storage contract lets a reader keep an object while a Put replaces it or
// a Delete removes it, so the store opens objects with delete sharing and
// replaces and deletes them with POSIX semantics: the name moves or goes at
// once and an open reader keeps the version it opened.

// shareAll is the share mode of every handle the store opens.
const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// openShared opens name for reading without locking its name.
func openShared(name string) (*os.File, error) {
	return createFile(name, windows.GENERIC_READ, windows.OPEN_EXISTING, "open")
}

// createShared creates name, which must not exist, for reading and writing
// without locking its name.
func createShared(name string) (*os.File, error) {
	return createFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW, "open")
}

func createFile(name string, access, mode uint32, op string) (*os.File, error) {
	h, err := create(name, access, mode, op)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

// create is CreateFile with the store's share mode. A file that has been
// deleted but whose name lingers until another handle on it closes (a
// delete without POSIX semantics, by the store's fallback or by another
// program) fails with ERROR_ACCESS_DENIED, which is indistinguishable
// from a permission failure but for the call's NTSTATUS,
// STATUS_DELETE_PENDING; that file is gone, so it is reported as not
// existing.
func create(name string, access, mode uint32, op string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(longPath(name))
	if err != nil {
		return 0, &fs.PathError{Op: op, Path: name, Err: err}
	}
	// The NTSTATUS is per thread: read it on the thread that made the call.
	runtime.LockOSThread()
	h, err := windows.CreateFile(p, access, shareAll, nil, mode, windows.FILE_ATTRIBUTE_NORMAL, 0)
	var status windows.NTStatus
	if err != nil {
		status = lastNtStatus()
	}
	runtime.UnlockOSThread()
	switch {
	case err == nil:
		return h, nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED) && status == windows.STATUS_DELETE_PENDING:
		err = windows.ERROR_FILE_NOT_FOUND
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		err = fmt.Errorf("%w (%s)", err, status)
	}
	return 0, &fs.PathError{Op: op, Path: name, Err: err}
}

var procRtlGetLastNtStatus = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlGetLastNtStatus")

// lastNtStatus is the NTSTATUS of the calling thread's last failed call.
func lastNtStatus() windows.NTStatus {
	if procRtlGetLastNtStatus.Find() != nil {
		return 0
	}
	r, _, _ := procRtlGetLastNtStatus.Call()
	return windows.NTStatus(uint32(r)) // #nosec G115 -- an NTSTATUS is 32 bits
}

// openForDelete opens name with the DELETE access a rename or a delete by
// handle needs.
func openForDelete(name, op string) (windows.Handle, error) {
	return create(name, windows.DELETE|windows.SYNCHRONIZE, windows.OPEN_EXISTING, op)
}

// posixUnsupported reports a filesystem (FAT, say) or Windows version
// without POSIX rename or delete semantics.
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

// replaceFile renames src to dst, replacing dst even while readers have it
// open. Where POSIX semantics are not available it falls back to
// os.Rename, which fails while dst is open.
func replaceFile(src, dst string) error {
	err := renameByHandle(src, dst, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
	if err != nil && posixUnsupported(err) {
		return os.Rename(src, dst)
	}
	return err
}

// renameNoReplace renames src to dst unless dst exists (fs.ErrExist), in
// one atomic step: the rename without REPLACE_IF_EXISTS, or MoveFileEx
// without MOVEFILE_REPLACE_EXISTING where POSIX semantics are not
// available.
func renameNoReplace(src, dst string) error {
	err := renameByHandle(src, dst, windows.FILE_RENAME_POSIX_SEMANTICS)
	if err != nil && posixUnsupported(err) {
		from, ferr := windows.UTF16PtrFromString(longPath(src))
		to, terr := windows.UTF16PtrFromString(longPath(dst))
		if ferr != nil || terr != nil {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errors.Join(ferr, terr)}
		}
		if err := windows.MoveFileEx(from, to, 0); err != nil {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
		}
		return nil
	}
	return err
}

// renameByHandle renames src to dst with SetFileInformationByHandle
// (FileRenameInfoEx) and flags.
func renameByHandle(src, dst string, flags uint32) error {
	name, err := windows.UTF16FromString(longPath(dst))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	h, err := openForDelete(src, "rename")
	if err != nil {
		return err
	}
	var info fileRenameInfo
	nameBytes := (len(name) - 1) * 2 // without the terminating NUL
	buf := make([]byte, int(unsafe.Offsetof(info.FileName))+len(name)*2)
	ri := (*fileRenameInfo)(unsafe.Pointer(&buf[0])) // #nosec G103 -- FILE_RENAME_INFO over a buffer sized for its name
	ri.Flags = flags
	ri.FileNameLength = uint32(nameBytes)                                                            // #nosec G115 -- a path, far below 4 GiB
	copy(unsafe.Slice(&ri.FileName[0], len(name)), name)                                             // #nosec G103 -- within buf, as above
	err = windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(len(buf))) // #nosec G115 -- as above
	_ = windows.CloseHandle(h)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

// removeFile deletes name, even while readers have it open, and frees the
// name at once. Where POSIX semantics are not available it falls back to
// os.Remove.
func removeFile(name string) error {
	h, err := openForDelete(name, "remove")
	if err != nil {
		return err
	}
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	err = windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags))) // #nosec G103 -- FILE_DISPOSITION_INFO_EX is one DWORD
	_ = windows.CloseHandle(h)
	if err != nil && posixUnsupported(err) {
		return os.Remove(name)
	}
	if err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	return nil
}

// tryLock takes an exclusive lock on f without waiting, reporting whether
// it got it. The lock lasts until f is closed or its process exits.
func tryLock(f *os.File) (bool, error) {
	var ol windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// longPath returns path in the form the raw Win32 calls above need to
// reach it when it is long. The os package does the same for its own calls
// (os.fixLongPath): on Windows 10 1703 and later the Go runtime opts the
// process into long paths and neither is needed, but on the older versions
// Go supports (Windows Server 2016, say) a path of 248 characters or more
// works only with the extended \\?\ prefix, or \\?\UNC\ for a share. The
// store's paths are absolute and clean, as the prefix requires.
func longPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}
