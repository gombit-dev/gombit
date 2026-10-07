//go:build !windows

package atomicfile

import (
	"errors"
	"os"
	"syscall"
)

// replace renames src over dst: rename(2), which replaces dst atomically
// (so AllowNonAtomic changes nothing here).
func replace(src, dst string, _ options) error { return os.Rename(src, dst) }

// syncDir fsyncs dir, so that a rename in it reaches stable storage
// (fsync(2): syncing a file does not sync the entry naming it). A
// filesystem that cannot sync a directory reports EINVAL; that is not an
// error here, and the rename is then visible but not known to be durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- the directory of the file being written
	if err != nil {
		return err
	}
	err = d.Sync()
	if errors.Is(err, syscall.EINVAL) {
		err = nil
	}
	return errors.Join(err, d.Close())
}
