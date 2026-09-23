package resourcegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gombit-dev/gombit/migrations"
)

// PlatformDBRel is the app-relative path of the generated file whose AutoMigrate
// call is the runtime desired-state source of truth (the models the app persists
// and migrates on start). Callers surface it in messages about that file.
func PlatformDBRel() string { return platformDBRel }

// AutoMigrateModels returns the models registered in the app's AutoMigrate call
// (internal/platform/database.go under workDir). ok is false when the app has no
// such file — a non-scaffolded layout — so callers can skip AutoMigrate-based
// checks instead of failing. It never mutates the tree.
func AutoMigrateModels(workDir string) (models []migrations.Model, ok bool, err error) {
	path := filepath.Join(workDir, filepath.FromSlash(platformDBRel))
	// #nosec G304 -- application file under the caller-provided work dir
	src, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("resourcegen: read %s: %w", platformDBRel, err)
	}
	models, err = CollectAutoMigrateModels(src)
	if err != nil {
		return nil, false, err
	}
	return models, true, nil
}
