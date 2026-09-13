package migrations

import (
	"bytes"
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
	Stdout       io.Writer
	Stderr       io.Writer

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

	absWorkDir, err := filepath.Abs(opts.WorkDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve work dir: %w", err)
	}
	migrationDir := opts.MigrationDir
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(absWorkDir, migrationDir)
	}

	// Loaded (and the "nothing to do" case rejected) before MkdirAll, so an
	// invocation that ends up with nothing to migrate doesn't leave behind an
	// empty migration directory as a side effect of failing.
	registered, err := LoadRegistry(migrationDir)
	if err != nil {
		return err
	}
	// allModels, not opts.Models, is the desired schema: it also carries
	// forward every model an earlier makemigrations call registered, so this
	// invocation only needs to name what's new. Without this, Atlas would
	// see anything not repeated here as schema drift and drop it (#97).
	allModels := SubtractModels(MergeModels(registered, opts.Models), opts.ForgetModels)
	if len(allModels) == 0 {
		return errors.New("migrations: no models to migrate: nothing in the registry and no --model given")
	}

	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		return fmt.Errorf("migrations: create migration dir: %w", err)
	}

	tmpRoot := filepath.Join(absWorkDir, ".gombit")
	_, statErr := os.Stat(tmpRoot)
	tmpRootExisted := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("migrations: inspect temp root: %w", statErr)
	}
	if err := os.MkdirAll(tmpRoot, 0o750); err != nil {
		return fmt.Errorf("migrations: create temp root: %w", err)
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, "makemigrations-*")
	if err != nil {
		return fmt.Errorf("migrations: create temp dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
		if !tmpRootExisted {
			_ = os.Remove(tmpRoot)
		}
	}()

	loaderDir := filepath.Join(tmpDir, "loader")
	if err := os.MkdirAll(loaderDir, 0o750); err != nil {
		return fmt.Errorf("migrations: create loader dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(loaderDir, "main.go"), []byte(loaderSource(opts.Driver, allModels)), 0o600); err != nil {
		return fmt.Errorf("migrations: write loader: %w", err)
	}

	loaderRel, err := filepath.Rel(absWorkDir, loaderDir)
	if err != nil {
		return fmt.Errorf("migrations: resolve loader path: %w", err)
	}
	var schema bytes.Buffer
	goArgs := []string{"run", "-mod=mod", "./" + filepath.ToSlash(loaderRel)}
	if err := opts.runner.Run(ctx, absWorkDir, "go", goArgs, &schema, opts.Stderr); err != nil {
		return fmt.Errorf("migrations: load gorm schema: %w", err)
	}

	schemaPath := filepath.Join(tmpDir, "schema.sql")
	if err := os.WriteFile(schemaPath, schema.Bytes(), 0o600); err != nil {
		return fmt.Errorf("migrations: write schema: %w", err)
	}

	atlasPath := filepath.Join(tmpDir, "atlas.hcl")
	if err := os.WriteFile(atlasPath, []byte(atlasHCL(schemaPath, migrationDir, devURL(opts.Driver))), 0o600); err != nil {
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
	if err := opts.runner.Run(ctx, absWorkDir, opts.AtlasBinary, args, opts.Stdout, opts.Stderr); err != nil {
		return fmt.Errorf("migrations: atlas migrate diff: %w", err)
	}
	if err := SaveRegistry(migrationDir, allModels); err != nil {
		return err
	}
	// A diff that renames a field shows up as a drop + add, which Atlas turns
	// into a table rebuild that can lose data or fail to apply to a non-empty
	// table. Review the generated SQL; if you hand-edit it, `gombit db hash`
	// refreshes atlas.sum so the migration still applies (#219).
	_, _ = fmt.Fprintln(opts.Stdout, "Review the generated migration before applying. If you hand-edit it, run 'gombit db hash' to refresh atlas.sum before 'gombit db migrate'.")
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
	switch opts.Driver {
	case config.DatabaseDriverSQLite, config.DatabaseDriverPostgres, config.DatabaseDriverMySQL:
	default:
		return fmt.Errorf("migrations: unsupported driver %q", opts.Driver)
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
