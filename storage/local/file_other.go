//go:build !windows

package local

import "os"

// Elsewhere an open file does not lock its name: a rename replaces it and a
// remove deletes it while readers keep the version they opened.

func openShared(name string) (*os.File, error) {
	return os.Open(name) // #nosec G304 -- a file under the store's own root
}

func createShared(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- as above
}

func replaceFile(src, dst string) error { return os.Rename(src, dst) }

func removeFile(name string) error { return os.Remove(name) }
