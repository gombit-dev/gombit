//go:build !windows

package atomicfile

import "os"

// replace renames src over dst: rename(2), which replaces dst atomically.
func replace(src, dst string) error { return os.Rename(src, dst) }
