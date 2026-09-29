//go:build !windows && !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package local

import "os"

// tryLock: this platform has no file lock the store uses, so it cannot
// tell a live store's work directory from a dead one's and sweeps nothing.
func tryLock(*os.File) (bool, error) { return false, errNoLock }
