package upgrade

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// frameworkTables are the framework models' own tables (not the join
// tables they declare), by model as a schema entry names it. A test holds
// them to what GORM reads from the models.
var frameworkTables = map[string]string{
	"auth.User":            "users",
	"auth.RefreshToken":    "refresh_tokens",
	"auth.Group":           "groups",
	"auth.Permission":      "permissions",
	"storage/claims.Claim": "storage_claims",
}

// FrameworkTable is a framework model's own table ("" for a model this
// gombit does not have), by model as a schema entry names it
// (auth.RefreshToken).
func FrameworkTable(model string) string { return frameworkTables[model] }

// defaultMigrationDir is gombit db's migration directory (--dir).
const defaultMigrationDir = "database/migrations"

// SchemaChangeKind is how a release changes a framework-owned table.
//
// An entry is about one table. Only the model's own table being added or
// removed adds or removes the model (the registry's --model and
// --forget-model); a join table it declares (Join) changing, appearing or
// going away alters the model, as a column would.
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
	// Table is the table: the model's own, or a many-to-many join table
	// the model declares (Join).
	Table string `yaml:"table" json:"table"`
	// Join: Table is a join table Model declares (auth_user_groups for
	// auth.User). Its change alters Model; it never adds or removes it.
	Join bool `yaml:"join,omitempty" json:"join,omitempty"`
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
	if sc.Join {
		d = fmt.Sprintf("`%s` (join table `%s`): %s", sc.Model, sc.Table, sc.Change)
	}
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
// default database/migrations, as gombit db --dir): per change, whether
// the application's model registry lists the model and what to do, and the
// migration steps the upgrade needs, each command with the flags the next
// one needs to act on the same schema. It reads the registry only, and
// never touches a database.
func AnalyzeSchema(workDir, migrationDir string, releases []Release) (SchemaReport, error) {
	if migrationDir == "" {
		migrationDir = defaultMigrationDir
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
	// its AutoMigrate's call: a change to one it may have is reported as
	// needing a migration, never as not its table.
	mayHave := func(m migrations.Model) bool { return registered[m] || !report.RegistryFound }
	var add, forget []string
	migrate, destructive := false, false
	for _, r := range releases {
		for _, c := range r.Changes {
			for _, sc := range c.Schema {
				f := SchemaFinding{SchemaChange: sc, Release: r.Version, ChangeID: c.ID}
				model := sc.RegistryModel()
				f.Registered = registered[model]
				name := model.ImportPath + "." + model.TypeName
				what := sc.Table
				if sc.Join {
					what = "join table " + sc.Table
				}
				switch {
				case c.Kind != KindManual: // an optional feature's new table (validated: added, not a join table)
					f.Advice = fmt.Sprintf("Nothing to migrate unless the application adopts it: then add %s to its models (AutoMigrate in internal/platform) and generate its migration with --model %s.", sc.Model, name)
					if f.Registered {
						f.Advice = fmt.Sprintf("The registry lists %s, so the application's migrations carry %s.", sc.Model, sc.Table)
					}
				case sc.Join || sc.Change == SchemaAltered || (sc.Change == SchemaAdded && f.Registered):
					// The model changes: its next migration carries it.
					if !mayHave(model) {
						f.Advice = fmt.Sprintf("The registry does not list %s: the application has no %s to migrate.", sc.Model, what)
						break
					}
					f.Required, migrate = true, true
					f.Advice = fmt.Sprintf("The registry lists %s, so the next migration carries its %s %s.", sc.Model, sc.Change, what)
					if !f.Registered {
						f.Advice = fmt.Sprintf("With no model registry, whether the application has %s is its AutoMigrate's call: if it lists %s, the next migration carries its %s %s.", what, sc.Model, sc.Change, what)
					}
				case sc.Change == SchemaAdded:
					f.Required = true
					add = appendOnce(add, name)
					registered[model] = true
					f.Advice = fmt.Sprintf("Add %s to the application's models (AutoMigrate in internal/platform), and generate its table's migration with --model %s.", sc.Model, name)
				case mayHave(model): // the model's own table is removed: the model is gone
					f.Required = true
					forget = appendOnce(forget, name)
					delete(registered, model)
					f.Advice = fmt.Sprintf("Remove %s from the application's models, and generate the migration that drops %s with --forget-model %s, once its data is no longer needed.", sc.Model, sc.Table, name)
				default:
					f.Advice = fmt.Sprintf("The registry does not list %s: no table to drop.", sc.Model)
				}
				if f.Required && sc.Destructive {
					destructive = true
					f.Advice += " Destructive: back up or move the data first; gombit db plan asks for --allow."
				}
				report.Findings = append(report.Findings, f)
			}
		}
	}
	if len(add) == 0 && len(forget) == 0 && !migrate {
		return report, nil
	}
	if !report.RegistryFound {
		report.Steps = append(report.Steps, fmt.Sprintf("The application has no model registry (%s): it has generated no migration, so its tables come from AutoMigrate alone. Generate its first migration (gombit db makemigrations) before deploying with migrations.", report.Registry))
	}
	// One set of flags for every command, so the plan reviewed is the
	// migration generated, in the application's own directory.
	dir := ""
	if filepath.ToSlash(migrationDir) != defaultMigrationDir {
		dir = " --dir " + migrationDir
	}
	flags := dir
	for _, name := range add {
		flags += " --model " + name
	}
	for _, name := range forget {
		flags += " --forget-model " + name
	}
	if len(add) > 0 {
		report.Steps = append(report.Steps, "Add the new framework models to the application's AutoMigrate (internal/platform/database.go): "+strings.Join(add, ", ")+".")
	}
	if len(forget) > 0 {
		report.Steps = append(report.Steps, "Remove the framework models the release drops from the application's AutoMigrate: "+strings.Join(forget, ", ")+".")
	}
	allow := "If it reports a destructive or unsafe step, handle its data first and add --allow <its id>."
	if destructive {
		allow = "This upgrade declares a destructive change: gombit db plan reports its step; handle the data first, then add --allow <its id>."
	}
	report.Steps = append(report.Steps,
		"After the dependency change, review what the migration will do: gombit db plan"+flags+".",
		"Generate it, with the same flags: gombit db makemigrations upgrade_framework_schema"+flags+". "+allow,
		"Apply it in each environment with gombit db migrate"+dir+", as a reviewed deploy step. gombit upgrade never applies a migration.",
	)
	return report, nil
}

func appendOnce(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
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
		// Informational is for a table an application adopts with a
		// feature: a new model's own table. Anything else changes a table
		// an application may have, which the plan must ask it to migrate.
		if c.Kind == KindInformational && (sc.Change != SchemaAdded || sc.Join) {
			return fmt.Errorf("change %s: %s: an informational change only adds an optional feature's table; a %s %s needs a migration, so the change is manual", c.ID, sc.Model, sc.Change, sc.Table)
		}
		if seen[sc.Table] {
			return fmt.Errorf("change %s: table %s is listed twice", c.ID, sc.Table)
		}
		seen[sc.Table] = true
	}
	return nil
}
