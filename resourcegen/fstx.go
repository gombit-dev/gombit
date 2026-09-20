package resourcegen

import (
	"errors"
	"os"
	"path/filepath"
)

// fsTx applies a sequence of file writes that can be undone. Each write goes to a
// sibling temp file that is fsynced and atomically renamed over the target, so a
// failed or partial write (disk-full, quota, short write) never leaves the target
// truncated — the target holds either its old bytes or the complete new bytes,
// never anything in between. For every applied write it records whether the target
// existed (and its prior bytes + mode) and which parent directories it created, so
// rollback restores the tree to its pre-apply state. It backs make resource's
// atomic commit — the scaffold and the generated files land together or not at all.
type fsTx struct {
	steps []fsStep
}

type fsStep struct {
	path        string
	existed     bool
	prior       []byte
	priorMode   os.FileMode
	createdDirs []string // deepest first
}

// write records the target's prior state, creates any missing parent directories,
// and atomically replaces the target with content. A recorded step is appended only
// after the rename succeeds; a failure before that never mutated the target (the
// old bytes are intact) and undoes only the directories this call created.
func (tx *fsTx) write(path string, content []byte) error {
	info, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	var (
		prior []byte
		mode  = os.FileMode(0o644)
	)
	if existed {
		var readErr error
		prior, readErr = os.ReadFile(path) // #nosec G304 -- generator output path under the app work dir
		if readErr != nil {
			return readErr
		}
		mode = info.Mode().Perm()
	}
	created, err := mkdirAllTracked(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, content, mode); err != nil {
		removeDirs(created)
		return err
	}
	tx.steps = append(tx.steps, fsStep{path: path, existed: existed, prior: prior, priorMode: mode, createdDirs: created})
	return nil
}

// rollback undoes every applied write in reverse order: atomically restoring the
// prior bytes (and mode) of files that existed, removing files this transaction
// created, and pruning the directories it created (deepest first, only when empty).
func (tx *fsTx) rollback() error {
	var firstErr error
	for i := len(tx.steps) - 1; i >= 0; i-- {
		s := tx.steps[i]
		if s.existed {
			if err := writeFileAtomic(s.path, s.prior, s.priorMode); err != nil && firstErr == nil {
				firstErr = err
			}
		} else if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
		removeDirs(s.createdDirs)
	}
	tx.steps = nil
	return firstErr
}

// writeFileAtomic writes content to a temp file in the target's directory, fsyncs
// and closes it, sets mode, then renames it over path. The rename is atomic on the
// same filesystem, so path is never observed truncated; a failure anywhere before
// the rename leaves path untouched and removes the temp file.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gombit-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// If we return before the rename, the temp file must not linger. After a
	// successful rename tmpName no longer exists and this is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// mkdirAllTracked creates dir (and any missing parents) and returns the
// directories it actually created, deepest first, so rollback can remove exactly
// those.
func mkdirAllTracked(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return missing, nil
}

// removeDirs removes the given directories best-effort, deepest first; a non-empty
// directory (still holding files from another step) is left in place.
func removeDirs(dirs []string) {
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}
