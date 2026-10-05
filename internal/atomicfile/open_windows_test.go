package atomicfile

import (
	"os"

	"golang.org/x/sys/windows"
)

// openShared opens name for reading with delete sharing, as a reader that
// does not lock the name (storage/local's readers, an editor that shares
// delete) does.
func openShared(name string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}
