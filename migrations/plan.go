package migrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gombit-dev/gombit/config"
)

// Inspection is the pair of schema states a migration plan compares, as the
// HCL `atlas schema inspect` prints on the dev database. Package schemaplan
// classifies it. It stays raw here so this package, which generated apps'
// loaders compile, does not depend on the Atlas SQL libraries.
type Inspection struct {
	Driver config.DatabaseDriver
	// Current is the schema the migration directory builds. It is empty
	// before the first migration.
	Current []byte
	// Desired is the schema the models declare.
	Desired []byte
	// ForgetModels are the models this run stops tracking: their tables are
	// meant to be dropped.
	ForgetModels []Model
}

// Gate decides whether MakeMigrations may write the migration named name. It
// runs after the desired schema is loaded and before `atlas migrate diff`,
// once the directory holds a migration; a non-nil error refuses the write.
// schemaplan.Gate is the implementation the gombit CLI installs.
type Gate func(ctx context.Context, name string, in Inspection) error

// InspectOptions configures Inspect. The model fields mean what they mean for
// MakeMigrations: the desired schema is the persisted registry plus Models,
// minus ForgetModels.
type InspectOptions struct {
	WorkDir      string
	Driver       config.DatabaseDriver
	MigrationDir string
	AtlasBinary  string
	Models       []Model
	ForgetModels []Model
	Stderr       io.Writer

	runner commandRunner
}

// Inspect loads the desired schema from the models and inspects it and the
// migration directory with `atlas schema inspect`, without writing a
// migration. It is what `gombit db plan` classifies.
func Inspect(ctx context.Context, opts InspectOptions) (Inspection, error) {
	if ctx == nil {
		return Inspection{}, errors.New("migrations: nil context")
	}
	mopts := withDefaults(Options{
		WorkDir:      opts.WorkDir,
		Driver:       opts.Driver,
		MigrationDir: opts.MigrationDir,
		AtlasBinary:  opts.AtlasBinary,
		Models:       opts.Models,
		ForgetModels: opts.ForgetModels,
		Stderr:       opts.Stderr,
		runner:       opts.runner,
	})
	if err := validateDriver(mopts.Driver); err != nil {
		return Inspection{}, err
	}
	for _, model := range append(append([]Model(nil), mopts.Models...), mopts.ForgetModels...) {
		if err := validateModel(model); err != nil {
			return Inspection{}, err
		}
	}
	ws, allModels, err := prepareWorkspace(mopts)
	if err != nil {
		return Inspection{}, err
	}
	defer ws.cleanup()
	if err := ws.loadSchema(ctx, mopts, allModels); err != nil {
		return Inspection{}, err
	}
	return ws.inspect(ctx, mopts)
}

// workspace is the temporary directory a makemigrations or plan run works in:
// the generated Atlas loader, the desired-schema SQL it prints, and the Atlas
// config. It lives under <workdir>/.gombit so `go run` resolves the app module.
type workspace struct {
	absWorkDir   string
	migrationDir string
	tmpDir       string
	schemaPath   string
	cleanup      func()
}

// prepareWorkspace resolves the paths, loads and merges the model registry,
// and creates the temporary directory. It does not create the migration
// directory.
func prepareWorkspace(opts Options) (*workspace, []Model, error) {
	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return nil, nil, fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	migrationDir := opts.MigrationDir
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(absWorkDir, migrationDir)
	}

	// Loaded (and the "nothing to do" case rejected) before any directory is
	// created, so an invocation that ends up with nothing to migrate doesn't
	// leave an empty directory behind as a side effect of failing.
	registered, err := LoadRegistry(migrationDir)
	if err != nil {
		return nil, nil, err
	}
	// allModels, not opts.Models, is the desired schema: it also carries
	// forward every model an earlier makemigrations call registered, so this
	// invocation only needs to name what's new. Without this, Atlas would
	// see anything not repeated here as schema drift and drop it (#97).
	known := MergeModels(registered, opts.Models)
	if err := ensureForgetModelsTracked(known, opts.ForgetModels); err != nil {
		return nil, nil, err
	}
	allModels := SubtractModels(known, opts.ForgetModels)
	if len(allModels) == 0 {
		return nil, nil, errors.New("migrations: no models to migrate: nothing in the registry and no --model given")
	}

	tmpRoot := filepath.Join(absWorkDir, ".gombit")
	_, statErr := os.Stat(tmpRoot)
	tmpRootExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("migrations: inspect temp root: %w", statErr)
	}
	if err := os.MkdirAll(tmpRoot, 0o750); err != nil {
		return nil, nil, fmt.Errorf("migrations: create temp root: %w", err)
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "makemigrations-*")
	if err != nil {
		return nil, nil, fmt.Errorf("migrations: create temp dir: %w", err)
	}
	return &workspace{
		absWorkDir:   absWorkDir,
		migrationDir: migrationDir,
		tmpDir:       tmpDir,
		schemaPath:   filepath.Join(tmpDir, "schema.sql"),
		cleanup: func() {
			_ = os.RemoveAll(tmpDir)
			if !tmpRootExisted {
				_ = os.Remove(tmpRoot)
			}
		},
	}, allModels, nil
}

// loadSchema generates the Atlas Program Mode loader for models, runs it, and
// writes the desired-schema SQL it prints to ws.schemaPath.
func (ws *workspace) loadSchema(ctx context.Context, opts Options, models []Model) error {
	loaderDir := filepath.Join(ws.tmpDir, "loader")
	if err := os.MkdirAll(loaderDir, 0o750); err != nil {
		return fmt.Errorf("migrations: create loader dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(loaderDir, "main.go"), []byte(loaderSource(opts.Driver, models)), 0o600); err != nil {
		return fmt.Errorf("migrations: write loader: %w", err)
	}
	loaderRel, err := filepath.Rel(ws.absWorkDir, loaderDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve loader path: %w", err)
	}
	var schemaSQL bytes.Buffer
	goArgs := []string{"run", "-mod=mod", "./" + filepath.ToSlash(loaderRel)}
	if err := opts.runner.Run(ctx, ws.absWorkDir, "go", goArgs, &schemaSQL, opts.Stderr); err != nil {
		return fmt.Errorf("migrations: load gorm schema: %w", err)
	}
	if err := os.WriteFile(ws.schemaPath, schemaSQL.Bytes(), 0o600); err != nil {
		return fmt.Errorf("migrations: write schema: %w", err)
	}
	return nil
}

// hasMigrations reports whether the migration directory holds any up
// migration. Before the first one, the current schema is empty and every
// desired table is new, so there is nothing a gate could flag.
func (ws *workspace) hasMigrations() (bool, error) {
	files, err := ListMigrationFiles(ws.migrationDir)
	if err != nil {
		return false, err
	}
	return len(files) > 0, nil
}

// inspect inspects the desired schema and, once a migration exists, the
// migration directory. loadSchema must have run.
func (ws *workspace) inspect(ctx context.Context, opts Options) (Inspection, error) {
	in := Inspection{Driver: opts.Driver, ForgetModels: opts.ForgetModels}
	var err error
	if in.Desired, err = ws.inspectHCL(ctx, opts, "file://"+filepath.ToSlash(ws.schemaPath)); err != nil {
		return Inspection{}, fmt.Errorf("migrations: inspect the models' schema: %w", err)
	}
	has, err := ws.hasMigrations()
	if err != nil {
		return Inspection{}, err
	}
	if has {
		if in.Current, err = ws.inspectHCL(ctx, opts, "file://"+filepath.ToSlash(ws.migrationDir)); err != nil {
			return Inspection{}, fmt.Errorf("migrations: inspect the migration directory: %w (if a migration was edited by hand, run 'gombit db hash' first)", err)
		}
	}
	return in, nil
}

// inspectHCL runs `atlas schema inspect` on url against the dev database and
// returns the HCL it prints.
func (ws *workspace) inspectHCL(ctx context.Context, opts Options, url string) ([]byte, error) {
	var out, errOut bytes.Buffer
	args := []string{"schema", "inspect", "--url", url, "--dev-url", devURL(opts.Driver)}
	// Atlas prints its Community Edition notice on every run; keep stderr for
	// the error instead of repeating the notice on each inspection.
	if err := opts.runner.Run(ctx, ws.absWorkDir, opts.AtlasBinary, args, &out, &errOut); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return nil, fmt.Errorf("atlas schema inspect: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("atlas schema inspect: %w", err)
	}
	return out.Bytes(), nil
}

func validateDriver(driver config.DatabaseDriver) error {
	switch driver {
	case config.DatabaseDriverSQLite, config.DatabaseDriverPostgres, config.DatabaseDriverMySQL:
		return nil
	}
	return fmt.Errorf("migrations: unsupported driver %q", driver)
}
