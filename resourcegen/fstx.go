package resourcegen

import (
	"errors"
	"os"
	"path/filepath"
)

// fsTx applies a sequence of file writes that can be undone: for each write it
// records whether the file already existed (and its prior bytes) and which parent
// directories it had to create, so rollback restores the tree to its pre-apply
// state. It backs make resource's atomic commit — the scaffold and the generated
// files land together or not at all.
type fsTx struct {
	steps []fsStep
}

type fsStep struct {
	path        string
	existed     bool
	prior       []byte
	createdDirs []string // deepest first
}

// write records path's prior state, creates any missing parent directories, and
// writes content. A failure before the file is written undoes only the directories
// this call created; a recorded step is added only once the write succeeds.
func (tx *fsTx) write(path string, content []byte) error {
	prior, err := os.ReadFile(path) // #nosec G304 -- generator output path under the app work dir
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	created, err := mkdirAllTracked(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // generated source is a non-secret artifact
		removeDirs(created)
		return err
	}
	tx.steps = append(tx.steps, fsStep{path: path, existed: existed, prior: prior, createdDirs: created})
	return nil
}

// rollback undoes every recorded write in reverse order: restoring prior bytes for
// files that existed, removing files this transaction created, and pruning the
// directories it created (deepest first, only when empty).
func (tx *fsTx) rollback() error {
	var firstErr error
	for i := len(tx.steps) - 1; i >= 0; i-- {
		s := tx.steps[i]
		if s.existed {
			if err := os.WriteFile(s.path, s.prior, 0o644); err != nil && firstErr == nil { //nolint:gosec // restoring prior content
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
