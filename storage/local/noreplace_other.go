//go:build !linux && !darwin && !windows

package local

// renameNoReplace renames src to dst unless dst exists (fs.ErrExist): a
// hard link, which cannot replace an existing name, then the removal of
// src.
func renameNoReplace(src, dst string) error { return linkNoReplace(src, dst) }
