//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package local

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive lock on f without waiting, reporting whether
// it got it. The lock (flock) belongs to f's open file, so a second open of
// the same file conflicts even in the same process; it lasts until f is
// closed or its process exits.
func tryLock(f *os.File) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var lerr error
	if err := rc.Control(func(fd uintptr) {
		lerr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a file descriptor
	}); err != nil {
		return false, err
	}
	if errors.Is(lerr, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return lerr == nil, lerr
}
