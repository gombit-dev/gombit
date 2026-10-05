//go:build !windows

package atomicfile

import "os"

func openShared(name string) (*os.File, error) { return os.Open(name) } // #nosec G304 -- under t.TempDir()
