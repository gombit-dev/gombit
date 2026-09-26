package migrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gombit-dev/gombit/config"
)

const (
	defaultAtlasBinary  = "atlas"
	defaultMigrationDir = "database/migrations"
)

var goIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var migrationNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Model identifies one GORM model type imported by the generated Atlas loader.
type Model struct {
	ImportPath string `json:"import_path"`
	TypeName   string `json:"type_name"`
}

// Options configures MakeMigrations.
type Options struct {
	WorkDir      string
	Name         string
	Driver       config.DatabaseDriver
	MigrationDir string
	AtlasBinary  string
	// Models are the GORM models this invocation is declaring. They are
	// merged with whatever is already in the persisted registry
	// (RegistryPath) — not the entire desired schema by themselves — so a
	// migration for one new model does not implicitly drop tables for
	// models used in earlier migrations.
	Models []Model
	// ForgetModels removes models from the persisted registry (and this
	// invocation's desired schema), the explicit way to let Atlas propose
	// dropping a table that's genuinely going away.
	ForgetModels []Model
	// Renames, when non-empty, makes this a rename migration: Gombit emits a
	// native, data-preserving RENAME COLUMN instead of the drop+add table rebuild
	// `atlas migrate diff` would generate for a field rename. It is not combined
	// with model/schema diffing in the same run.
	Renames []Rename
	// TableRenames, like Renames, makes this a rename migration: Gombit emits
	// a native ALTER TABLE ... RENAME TO, applied before the column renames.
	// It may name the models the rename swaps (Models / ForgetModels): they
	// update the registry, and no model diff runs.
	TableRenames []TableRename
	// Gate, when set, may refuse the migration after inspecting the change
	// (see Gate). Nil writes whatever `atlas migrate diff` produces.
	Gate   Gate
	Stdout io.Writer
	Stderr io.Writer

	runner commandRunner
}

type commandRunner interface {
	Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error
}

type execRunner struct{}

// ParseModel parses a model spec in the form "import/path.TypeName".
func ParseModel(spec string) (Model, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Model{}, errors.New("migrations: empty model spec")
	}

	dot := strings.LastIndex(spec, ".")
	if dot <= 0 || dot == len(spec)-1 {
		return Model{}, fmt.Errorf("migrations: model %q must be import/path.TypeName", spec)
	}

	model := Model{
		ImportPath: spec[:dot],
		TypeName:   spec[dot+1:],
	}
	if strings.TrimSpace(model.ImportPath) == "" || strings.ContainsAny(model.ImportPath, " \t\r\n") {
		return Model{}, fmt.Errorf("migrations: model %q has invalid import path", spec)
	}
	if !goIdentifierPattern.MatchString(model.TypeName) {
		return Model{}, fmt.Errorf("migrations: model %q has invalid type name", spec)
	}
	return model, nil
}

// MakeMigrations generates an Atlas Program Mode loader and runs atlas migrate diff.
func MakeMigrations(ctx context.Context, opts Options) error {
	if ctx == nil {
		return errors.New("migrations: nil context")
	}

	opts = withDefaults(opts)
	opts.Name = strings.TrimSpace(opts.Name)
	if err := validateOptions(opts); err != nil {
		return err
	}

	// A rename is a focused, data-preserving operation with its own SQL path; it
	// does not diff models, so it never runs alongside model add/drop in one call.
	if len(opts.Renames) > 0 || len(opts.TableRenames) > 0 {
		return makeRenameMigration(ctx, opts)
	}

	ws, allModels, err := prepareWorkspace(opts)
	if err != nil {
		return err
	}
	defer ws.cleanup()
	if err := os.MkdirAll(ws.migrationDir, 0o750); err != nil {
		return fmt.Errorf("migrations: create migration dir: %w", err)
	}
	if err := ws.loadSchema(ctx, opts, allModels); err != nil {
		return err
	}

	// Gate before writing. The first migration only creates tables, so there is
	// nothing to classify; after that, the gate (schemaplan.Gate in the CLI)
	// refuses a destructive or unsafe change nothing acknowledged (#309).
	var acknowledged []string
	if opts.Gate != nil {
		has, err := ws.hasMigrations()
		if err != nil {
			return err
		}
		if has {
			in, err := ws.inspect(ctx, opts)
			if err != nil {
				return err
			}
			if acknowledged, err = opts.Gate(ctx, opts.Name, in); err != nil {
				return err
			}
		}
	}
	before, err := migrationFileSet(ws.migrationDir)
	if err != nil {
		return err
	}

	atlasPath := filepath.Join(ws.tmpDir, "atlas.hcl")
	if err := os.WriteFile(atlasPath, []byte(atlasHCL(ws.schemaPath, ws.migrationDir, devURL(opts.Driver))), 0o600); err != nil {
		return fmt.Errorf("migrations: write atlas config: %w", err)
	}

	args := []string{
		"migrate",
		"diff",
		opts.Name,
		"--env",
		"gombit",
		"--config",
		"file://" + filepath.ToSlash(atlasPath),
	}
	hints := newHintWriter(opts.Stderr)
	err = opts.runner.Run(ctx, ws.absWorkDir, opts.AtlasBinary, args, opts.Stdout, hints)
	hints.Flush()
	if err != nil {
		return fmt.Errorf("migrations: atlas migrate diff: %w", err)
	}
	// Persist the acknowledgement in the migration itself, so the change
	// passes `gombit db lint` in CI without anyone repeating --allow.
	written, err := newMigrationFiles(ws.migrationDir, before)
	if err != nil {
		return err
	}
	if err := writeAllowDirectives(ctx, opts, ws.absWorkDir, ws.migrationDir, written, acknowledged); err != nil {
		return err
	}
	if err := SaveRegistry(ws.migrationDir, allModels); err != nil {
		return err
	}
	// A diff that renames a field shows up as a drop + add, which Atlas turns
	// into a table rebuild that can lose data or fail to apply to a non-empty
	// table. For a rename, generate a data-preserving migration with
	// `gombit db makemigrations <name> --rename table.old:new` instead (#299).
	_, _ = fmt.Fprintln(opts.Stdout, "Review the generated migration before applying. If it drops and re-adds a column you meant to rename, regenerate it with 'gombit db makemigrations <name> --rename table.old_column:new_column' to preserve the data.")
	return nil
}

func withDefaults(opts Options) Options {
	if opts.WorkDir == "" {
		opts.WorkDir = "."
	}
	if opts.MigrationDir == "" {
		opts.MigrationDir = defaultMigrationDir
	}
	if opts.AtlasBinary == "" {
		opts.AtlasBinary = defaultAtlasBinary
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.runner == nil {
		opts.runner = execRunner{}
	}
	return opts
}

func validateOptions(opts Options) error {
	if strings.TrimSpace(opts.Name) == "" {
		return errors.New("migrations: migration name is required")
	}
	if !migrationNamePattern.MatchString(opts.Name) {
		return errors.New("migrations: migration name must contain only letters, numbers, underscores, or hyphens and must not start with a hyphen")
	}
	if err := validateDriver(opts.Driver); err != nil {
		return err
	}
	// A migration can validly carry zero --model flags when the persisted
	// registry already has entries (this run only picks up field changes on
	// already-known models), so "no models at all" is checked later against
	// the merged set, not here.
	for _, model := range opts.Models {
		if err := validateModel(model); err != nil {
			return err
		}
	}
	for _, model := range opts.ForgetModels {
		if err := validateModel(model); err != nil {
			return err
		}
	}
	if len(opts.Renames) > 0 || len(opts.TableRenames) > 0 {
		// A rename generates SQL directly rather than diffing models. A column
		// rename keeps its model, so --model/--forget-model beside it would be
		// silently ignored. A table rename is a model rename: the models it
		// names swap in the registry.
		if len(opts.TableRenames) == 0 && (len(opts.Models) > 0 || len(opts.ForgetModels) > 0) {
			return errors.New("migrations: --rename cannot be combined with --model or --forget-model; generate the rename on its own (a table rename takes them, to swap the renamed model in the registry)")
		}
		if err := validateRenameSet(opts.TableRenames, opts.Renames); err != nil {
			return err
		}
	}
	return nil
}

// ensureForgetModelsTracked rejects a --forget-model that names a model nothing
// tracks. SubtractModels silently drops an unknown entry, so without this the run
// is a no-op that exits 0 with no DROP — the flag looks like it did nothing. A
// typo in the import path is the common cause (#300).
func ensureForgetModelsTracked(tracked, forget []Model) error {
	if len(forget) == 0 {
		return nil
	}
	have := make(map[Model]bool, len(tracked))
	for _, model := range tracked {
		have[model] = true
	}
	for _, model := range forget {
		if !have[model] {
			return fmt.Errorf("migrations: --forget-model %s.%s is not tracked (not in the model registry); nothing to forget", model.ImportPath, model.TypeName)
		}
	}
	return nil
}

func validateModel(model Model) error {
	if strings.TrimSpace(model.ImportPath) == "" || !goIdentifierPattern.MatchString(model.TypeName) {
		return fmt.Errorf("migrations: invalid model %#v", model)
	}
	return nil
}

func (execRunner) Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	// #nosec G204,G702 -- migrations intentionally executes the configured Atlas CLI binary.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func atlasHCL(schemaPath string, migrationDir string, dev string) string {
	return fmt.Sprintf(`env "gombit" {
  src = %q
  dev = %q
  migration {
    dir = %q
  }
  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}
`, "file://"+filepath.ToSlash(schemaPath), dev, "file://"+filepath.ToSlash(migrationDir))
}

func loaderSource(driver config.DatabaseDriver, models []Model) string {
	var b strings.Builder
	b.WriteString("package main\n\n")
	b.WriteString("import (\n")
	b.WriteString("\t\"fmt\"\n")
	b.WriteString("\t\"io\"\n")
	b.WriteString("\t\"os\"\n\n")
	b.WriteString("\t\"ariga.io/atlas-provider-gorm/gormschema\"\n")
	for i, model := range models {
		fmt.Fprintf(&b, "\tmodel%d %q\n", i, model.ImportPath)
	}
	b.WriteString(")\n\n")
	b.WriteString("func main() {\n")
	fmt.Fprintf(&b, "\tstmts, err := gormschema.New(%q).Load(\n", atlasDialect(driver))
	for i, model := range models {
		fmt.Fprintf(&b, "\t\t&model%d.%s{},\n", i, model.TypeName)
	}
	b.WriteString("\t)\n")
	b.WriteString("\tif err != nil {\n")
	b.WriteString("\t\tfmt.Fprintf(os.Stderr, \"failed to load gorm schema: %v\\n\", err)\n")
	b.WriteString("\t\tos.Exit(1)\n")
	b.WriteString("\t}\n")
	b.WriteString("\t_, _ = io.WriteString(os.Stdout, stmts)\n")
	b.WriteString("}\n")
	return b.String()
}

func atlasDialect(driver config.DatabaseDriver) string {
	return string(driver)
}

func devURL(driver config.DatabaseDriver) string {
	switch driver {
	case config.DatabaseDriverSQLite:
		return "sqlite://file?mode=memory&_fk=1"
	case config.DatabaseDriverPostgres:
		return "docker://postgres/15/dev?search_path=public"
	case config.DatabaseDriverMySQL:
		return "docker://mysql/8/dev"
	default:
		return ""
	}
}
