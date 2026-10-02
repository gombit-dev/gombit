package local

import (
	"os"
)

// linkNoReplace publishes src at dst unless dst exists (fs.ErrExist): a
// hard link fails atomically on an existing name. A filesystem without
// hard links fails the call (it has no atomic create-if-absent). Once
// linked, the object is published: if src then cannot be removed, the
// result is a *leftoverError, which reports a success with a stray
// temporary file, not a failure.
func linkNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		return err
	}
	if err := removeTempHook(src); err != nil {
		return &leftoverError{path: src, err: err}
	}
	return nil
}

// removeTempHook is os.Remove, replaceable by tests.
var removeTempHook = os.Remove

// leftoverError is a publish that succeeded but left its temporary file
// behind (a hard link published it; removing the temporary name failed).
// The file is in the store's work directory, which a sweep removes once
// the store is gone.
type leftoverError struct {
	path string
	err  error
}

func (e *leftoverError) Error() string {
	return "local storage: published, but the temporary file " + e.path + " could not be removed: " + e.err.Error()
}

func (e *leftoverError) Unwrap() error { return e.err }

// renameError is a failed no-replace rename, as os.Rename reports one.
type renameError = os.LinkError

// renameNoReplaceHook is renameNoReplace, replaceable by tests.
var renameNoReplaceHook = renameNoReplace
