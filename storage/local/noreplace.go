package local

import (
	"os"
)

// linkNoReplace publishes src at dst unless dst exists (fs.ErrExist): a
// hard link fails atomically on an existing name. src is removed once
// linked; a filesystem without hard links fails the call (it has no
// atomic create-if-absent).
func linkNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// renameError is a failed no-replace rename, as os.Rename reports one.
type renameError = os.LinkError
