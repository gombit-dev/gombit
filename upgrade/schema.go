package upgrade

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gombit-dev/gombit/internal/frameworkmod"
	"github.com/gombit-dev/gombit/migrations"
)

// Framework-owned schema: the tables the framework's own models (auth,
// storage claims) define in an application's database. An application's
// migrations create them, from its model registry, like its own tables, so
// a framework release that changes one needs a migration the application
// generates, reviews and applies. The upgrade tooling lists those steps; it
// never runs a migration.

// FrameworkModels are the framework models whose tables an application's
// migrations carry, as its model registry names them. A test holds the
// list to every Models() the framework exports.
var FrameworkModels = []migrations.Model{
	{ImportPath: frameworkmod.ModulePath + "/auth", TypeName: "User"},
	{ImportPath: frameworkmod.ModulePath + "/auth", TypeName: "RefreshToken"},
	{ImportPath: frameworkmod.ModulePath + "/auth", TypeName: "Group"},
	{ImportPath: frameworkmod.ModulePath + "/auth", TypeName: "Permission"},
	{ImportPath: frameworkmod.ModulePath + "/storage/claims", TypeName: "Claim"},
}

// SchemaChangeKind is how a release changes a framework-owned table.
type SchemaChangeKind string

const (
	// SchemaAdded: a new framework model, and its table.
	SchemaAdded SchemaChangeKind = "added"
	// SchemaAltered: the model's table changes (columns, types, indexes).
	SchemaAltered SchemaChangeKind = "altered"
	// SchemaRemoved: the framework no longer has the model; its table is
	// the application's to drop.
	SchemaRemoved SchemaChangeKind = "removed"
)

// SchemaChange is one framework-owned table a change adds, alters or
// removes.
type SchemaChange struct {
	// Model is the framework model, by its package within the framework
	// module and its type: auth.RefreshToken, storage/claims.Claim.
	Model string `yaml:"model" json:"model"`
	// Table is its table (a many-to-many join table is its own entry).
	Table string `yaml:"table" json:"table"`
	// Change is what happens to it.
	Change SchemaChangeKind `yaml:"change" json:"change"`
	// Destructive: the change drops or narrows something, so the migration
	// loses data unless the application acts first (gombit db plan then
	// asks for --allow).
	Destructive bool `yaml:"destructive,omitempty" json:"destructive,omitempty"`
}

var (
	schemaModel = regexp.MustCompile(`^[a-z][a-z0-9]*(/[a-z][a-z0-9]*)*\.[A-Z][A-Za-z0-9]*$`)
	schemaTable = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

func (sc SchemaChange) validate() error {
	if !schemaModel.MatchString(sc.Model) {
		return fmt.Errorf("schema model %q: want a framework package and type (auth.RefreshToken, storage/claims.Claim)", sc.Model)
	}
	if !schemaTable.MatchString(sc.Table) {
		return fmt.Errorf("schema %s: table %q, want a table name (a-z, 0-9, _)", sc.Model, sc.Table)
	}
	switch sc.Change {
	case SchemaAdded, SchemaAltered, SchemaRemoved:
	default:
		return fmt.Errorf("schema %s: change %q, want added, altered or removed", sc.Model, sc.Change)
	}
	if sc.Destructive && sc.Change == SchemaAdded {
		return fmt.Errorf("schema %s: an added table is not destructive", sc.Model)
	}
	return nil
}

// RegistryModel is the model as an application's model registry names it.
func (sc SchemaChange) RegistryModel() migrations.Model {
	i := strings.LastIndex(sc.Model, ".")
	return migrations.Model{ImportPath: frameworkmod.ModulePath + "/" + sc.Model[:i], TypeName: sc.Model[i+1:]}
}

// Describe is the change in one phrase: "auth.RefreshToken (refresh_tokens): altered".
func (sc SchemaChange) Describe() string {
	d := fmt.Sprintf("`%s` (`%s`): %s", sc.Model, sc.Table, sc.Change)
	if sc.Destructive {
		d += ", destructive"
	}
	return d
}

// SchemaFinding is a framework-owned table change on an upgrade's path, as
// it concerns the application.
type SchemaFinding struct {
	SchemaChange
	// Release and ChangeID say where the manifest declares it.
	Release  string `json:"release"`
	ChangeID string `json:"change_id"`
	// Required is whether the application must migrate for it: the change
	// is manual, and its model is one the application has (or, added, one
	// every application needs). An informational change's table is for an
	// application that adopts the feature it belongs to.
	Required bool `json:"required"`
	// Registered is whether the application's model registry lists the
	// model, so its migrations carry the table.
	Registered bool `json:"registered"`
	// Advice says what to do.
	Advice string `json:"advice"`
}

// SchemaReport is what an upgrade's framework-schema changes mean for an
// application's migrations.
type SchemaReport struct {
	// Registry is the application's model registry file, relative to the
	// application directory, and RegistryFound whether it exists: without
	// one the application has generated no migration yet.
	Registry      string `json:"registry"`
	RegistryFound bool   `json:"registry_found"`
	// Findings are the framework-schema changes on the path, in order.
	Findings []SchemaFinding `json:"findings"`
	// Steps are the migration steps the upgrade needs, in order: the
	// upgrade is not complete until they are done. Empty when no change
	// requires one. They are for the developer: gombit upgrade never
	// generates or applies a migration.
	Steps []string `json:"steps"`
}

// MigrationRequired reports whether the upgrade needs a migration.
func (r SchemaReport) MigrationRequired() bool { return len(r.Steps) > 0 }

// AnalyzeSchema reports what the framework-schema changes of releases (an
// upgrade's path, in order) mean for the application in workDir, whose
// migrations are in migrationDir (relative to workDir unless absolute;
// default database/migrations): per change, whether the application's
// model registry lists the model and what to do, and the migration steps
// the upgrade needs. It reads the registry only, and never touches a
// database.
func AnalyzeSchema(workDir, migrationDir string, releases []Release) (SchemaReport, error) {
	if migrationDir == "" {
		migrationDir = filepath.Join("database", "migrations")
	}
	abs := migrationDir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workDir, migrationDir)
	}
	report := SchemaReport{Registry: filepath.ToSlash(filepath.Join(migrationDir, "models.json")), Findings: []SchemaFinding{}, Steps: []string{}}
	registered := map[migrations.Model]bool{}
	switch _, err := os.Stat(migrations.RegistryPath(abs)); {
	case err == nil:
		report.RegistryFound = true
		models, err := migrations.LoadRegistry(abs)
		if err != nil {
			return SchemaReport{}, fmt.Errorf("upgrade: %w", err)
		}
		for _, m := range models {
			registered[m] = true
		}
	case !errors.Is(err, os.ErrNotExist):
		return SchemaReport{}, fmt.Errorf("upgrade: %w", err)
	}
	// Without a registry, which framework tables the application has is
	// unknown (its AutoMigrate decides): a change to one it may have is
	// reported as needing a migration, never as not its table.
	has := func(m migrations.Model) bool { return registered[m] || !report.RegistryFound }
	var add, forget []string
	migrate := false
	for _, r := range releases {
		for _, c := range r.Changes {
			for _, sc := range c.Schema {
				f := SchemaFinding{SchemaChange: sc, Release: r.Version, ChangeID: c.ID}
				model := sc.RegistryModel()
				f.Registered = registered[model]
				name := model.ImportPath + "." + model.TypeName
				switch {
				case c.Kind != KindManual:
					f.Advice = fmt.Sprintf("Nothing to migrate unless the application adopts it: then add %s to its models (AutoMigrate in internal/platform) and generate the migration (gombit db makemigrations upgrade_framework_schema --model %s).", sc.Model, name)
					if f.Registered {
						f.Advice = fmt.Sprintf("The registry lists %s, so its next migration carries the table (gombit db makemigrations).", sc.Model)
					}
				case sc.Change == SchemaAdded && !f.Registered:
					f.Required = true
					add = append(add, name)
					f.Advice = fmt.Sprintf("Add %s to the application's models (AutoMigrate in internal/platform), and generate its table's migration with --model %s.", sc.Model, name)
				case sc.Change == SchemaRemoved && has(model):
					f.Required = true
					forget = append(forget, name)
					f.Advice = fmt.Sprintf("Remove %s from the application's models, and generate the migration that drops %s with --forget-model %s, once its data is no longer needed.", sc.Model, sc.Table, name)
				case sc.Change == SchemaRemoved:
					f.Advice = fmt.Sprintf("The registry does not list %s: no table to drop.", sc.Model)
				case f.Registered: // added or altered, and in the registry
					f.Required = true
					migrate = true
					f.Advice = fmt.Sprintf("The registry lists %s, so the next migration carries the new %s.", sc.Model, sc.Table)
				case has(model): // altered, and no registry to say
					f.Required = true
					migrate = true
					f.Advice = fmt.Sprintf("With no model registry, whether the application has %s is its AutoMigrate's call: if it lists %s, the next migration carries the new %s.", sc.Table, sc.Model, sc.Table)
				default: // altered, not registered: the application does not have it
					f.Advice = fmt.Sprintf("The registry does not list %s: the application has no %s to migrate.", sc.Model, sc.Table)
				}
				if f.Required && sc.Destructive {
					f.Advice += " Destructive: back up or move the data first; gombit db plan asks for --allow."
				}
				report.Findings = append(report.Findings, f)
				// Later changes on the path see the registry as the steps
				// leave it: an added model is registered, a removed one
				// forgotten.
				if f.Required {
					registered[model] = sc.Change != SchemaRemoved
				}
			}
		}
	}
	if len(add) == 0 && len(forget) == 0 && !migrate {
		return report, nil
	}
	if !report.RegistryFound {
		report.Steps = append(report.Steps, fmt.Sprintf("The application has no model registry (%s): it has generated no migration, so its tables come from AutoMigrate alone. Generate its first migration (gombit db makemigrations) before deploying with migrations.", report.Registry))
	}
	cmd := "gombit db makemigrations upgrade_framework_schema"
	for _, name := range add {
		cmd += " --model " + name
	}
	for _, name := range forget {
		cmd += " --forget-model " + name
	}
	if len(add) > 0 {
		report.Steps = append(report.Steps, "Add the new framework models to the application's AutoMigrate (internal/platform/database.go): "+strings.Join(add, ", ")+".")
	}
	if len(forget) > 0 {
		report.Steps = append(report.Steps, "Remove the framework models the release drops from the application's AutoMigrate: "+strings.Join(forget, ", ")+".")
	}
	report.Steps = append(report.Steps,
		"After the dependency change, review what the migration will do: gombit db plan (a destructive or unsafe step needs --allow).",
		"Generate it: "+cmd+".",
		"Apply it in each environment with gombit db migrate, as a reviewed deploy step. gombit upgrade never applies a migration.",
	)
	return report, nil
}

// render writes the report for the plan.
func (r SchemaReport) render(b *strings.Builder) {
	fmt.Fprintf(b, "Framework schema changes: %d\n", len(r.Findings))
	for _, f := range r.Findings {
		label := "no migration"
		if f.Required {
			label = "migration required"
		}
		fmt.Fprintf(b, "  - [%s] %s (%s, %s)\n      %s\n", label, f.Describe(), f.Release, f.ChangeID, f.Advice)
	}
	if !r.MigrationRequired() {
		return
	}
	b.WriteString("Required migration steps (the upgrade is not complete until they are done):\n")
	for i, s := range r.Steps {
		fmt.Fprintf(b, "  %d. %s\n", i+1, s)
	}
}

// Render writes the report as text.
func (r SchemaReport) Render(w io.Writer) error {
	var b strings.Builder
	r.render(&b)
	_, err := io.WriteString(w, b.String())
	return err
}

// validateSchema checks a change's schema entries against the change.
func (c Change) validateSchema() error {
	if len(c.Schema) == 0 {
		return nil
	}
	if c.Area != "schema" {
		return fmt.Errorf("change %s: lists framework-schema changes, so its area is schema, not %s", c.ID, c.Area)
	}
	if c.Kind == KindAutomatic {
		return fmt.Errorf("change %s: lists framework-schema changes, which the developer migrates, so it is manual (or informational, for an optional feature's table), not automatic", c.ID)
	}
	seen := map[string]bool{}
	for _, sc := range c.Schema {
		if err := sc.validate(); err != nil {
			return fmt.Errorf("change %s: %w", c.ID, err)
		}
		if seen[sc.Table] {
			return fmt.Errorf("change %s: table %s is listed twice", c.ID, sc.Table)
		}
		seen[sc.Table] = true
	}
	return nil
}

// UseMigrationDir judges the plan's framework-schema changes against the
// application's migrations in migrationDir (relative to workDir unless
// absolute), for an application that keeps them elsewhere than
// database/migrations (gombit db --dir).
func (p *Plan) UseMigrationDir(workDir, migrationDir string) error {
	s, err := AnalyzeSchema(workDir, migrationDir, p.Releases)
	if err != nil {
		return err
	}
	p.Schema = s
	return nil
}

// UseMigrationDir is Plan.UseMigrationDir for what an upgrade applied.
func (a *Applied) UseMigrationDir(workDir, migrationDir string) error {
	if err := a.Plan.UseMigrationDir(workDir, migrationDir); err != nil {
		return err
	}
	a.MigrationRequired = a.Plan.Schema.MigrationRequired()
	return nil
}
