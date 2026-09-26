// Package schemaplan classifies the schema change a migration would make
// before it is written (SCHEMA-2, #309). It evaluates the HCL that
// `atlas schema inspect` prints for the migration directory and for the
// models, diffs the two with Atlas's own differ, and ranks every change by what
// it can do to existing rows. Nothing is parsed out of migration SQL.
//
// It is a separate package so the Atlas SQL libraries stay out of package
// migrations, which generated apps' loaders compile.
package schemaplan

import (
	"context"
	"fmt"
	"io"
	"strings"

	"ariga.io/atlas/sql/mysql"
	"ariga.io/atlas/sql/postgres"
	"ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqlite"
	gormschema "gorm.io/gorm/schema"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
)

// SchemaPlan is the classified difference between the schema the migration
// directory builds and the schema the models declare: what the next
// makemigrations would write.
type SchemaPlan struct {
	Driver config.DatabaseDriver `json:"driver"`
	Steps  []PlanStep            `json:"steps"`
	// forgottenTables are the tables of the models this run forgets, under
	// GORM's default naming: --forget-model is the explicit request to drop
	// them. A custom TableName or a join table is not in the set, so its drop
	// still needs --allow.
	forgottenTables map[string]bool
}

// Build classifies an inspection.
func Build(in migrations.Inspection) (SchemaPlan, error) {
	current, desired := &schema.Realm{}, &schema.Realm{}
	if err := evalHCL(in.Driver, in.Current, current); err != nil {
		return SchemaPlan{}, fmt.Errorf("schemaplan: read the migration directory's schema: %w", err)
	}
	if err := evalHCL(in.Driver, in.Desired, desired); err != nil {
		return SchemaPlan{}, fmt.Errorf("schemaplan: read the models' schema: %w", err)
	}
	alignSchemas(current, desired)
	changes, err := differ(in.Driver).RealmDiff(current, desired)
	if err != nil {
		return SchemaPlan{}, fmt.Errorf("schemaplan: diff schemas: %w", err)
	}
	steps := append([]PlanStep{}, classifyChanges(in.Driver, changes)...)
	for i := range steps {
		if steps[i].Code == StepDropTable {
			steps[i].Hint = renameTableHint(steps[i], modelFlags(in))
		}
	}
	return SchemaPlan{
		Driver:          in.Driver,
		Steps:           steps,
		forgottenTables: defaultTableNames(in.ForgetModels),
	}, nil
}

// modelFlags renders the --model / --forget-model flags of an inspection, so
// a suggested command reproduces the run's model set.
func modelFlags(in migrations.Inspection) string {
	var b strings.Builder
	for _, m := range in.NewModels {
		fmt.Fprintf(&b, " --model %s.%s", m.ImportPath, m.TypeName)
	}
	for _, m := range in.ForgetModels {
		fmt.Fprintf(&b, " --forget-model %s.%s", m.ImportPath, m.TypeName)
	}
	return b.String()
}

// Acknowledge marks the steps an --allow entry covers. An entry is a step ID
// (drop_column:products.name) or a bare code (drop_column) that covers every
// step with that code. The drop of a forgotten model's table is acknowledged
// too. It returns the entries that matched no step, which usually means a typo.
func (p *SchemaPlan) Acknowledge(allow []string) (unmatched []string) {
	used := map[string]bool{}
	ids := map[string]bool{}
	for _, a := range allow {
		ids[strings.TrimSpace(a)] = true
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		if ids[s.ID] {
			used[s.ID] = true
			s.Acknowledged = true
		}
		if ids[s.Code] {
			used[s.Code] = true
			s.Acknowledged = true
		}
		// A forgotten model's table drop is what --forget-model asks for,
		// but only when the plan creates no table: any new table may be its
		// rename, whose rows the drop would lose, so then it needs
		// --rename-table or an explicit --allow.
		if s.Code == StepDropTable && p.forgottenTables[s.Table] && len(s.createdInPlan) == 0 {
			s.Acknowledged = true
		}
	}
	for _, a := range allow {
		if a = strings.TrimSpace(a); !used[a] {
			unmatched = append(unmatched, a)
		}
	}
	return unmatched
}

// acknowledgedIDs are the destructive or unsafe steps an --allow entry or
// --forget-model acknowledged: what makemigrations records in the migration.
func (p SchemaPlan) acknowledgedIDs() []string {
	var ids []string
	for _, s := range p.Steps {
		if s.Acknowledged && (s.Severity == SeverityDestructive || s.Severity == SeverityUnsafe) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// Unacknowledged returns the destructive or unsafe steps nothing acknowledged.
func (p SchemaPlan) Unacknowledged() []PlanStep {
	var out []PlanStep
	for _, s := range p.Steps {
		if s.NeedsAcknowledgement() {
			out = append(out, s)
		}
	}
	return out
}

// Gate is the migrations.Gate the gombit CLI installs on makemigrations and
// make resource: it classifies the change and refuses a migration with a
// destructive or unsafe step that allow does not acknowledge, printing the
// plan to stderr.
func Gate(allow []string, stderr io.Writer) migrations.Gate {
	if stderr == nil {
		stderr = io.Discard
	}
	return func(_ context.Context, name string, in migrations.Inspection) ([]string, error) {
		plan, err := Build(in)
		if err != nil {
			return nil, err
		}
		for _, a := range plan.Acknowledge(allow) {
			_, _ = fmt.Fprintf(stderr, "warning: --allow %s matched no change in the plan\n", a)
		}
		pending := plan.Unacknowledged()
		if len(pending) == 0 {
			return plan.acknowledgedIDs(), nil
		}
		WritePlan(stderr, plan)
		return nil, fmt.Errorf("migrations: %d destructive or unsafe change(s) need acknowledgement, so no migration was written; review them with 'gombit db plan', handle the data, then write this migration with:\n  %s", len(pending), retryCommand(name, in, pending))
	}
}

// retryCommand is the makemigrations invocation that reproduces a refused run
// with its steps acknowledged. A refused run saves no registry, so the models
// it was adding (make resource's new model among them) and forgetting have to
// be named again.
func retryCommand(name string, in migrations.Inspection, pending []PlanStep) string {
	var b strings.Builder
	b.WriteString("gombit db makemigrations ")
	b.WriteString(name)
	b.WriteString(modelFlags(in))
	for _, s := range pending {
		b.WriteString(" --allow ")
		b.WriteString(s.ID)
	}
	return b.String()
}

// WritePlan renders a plan for a terminal. Destructive and unsafe steps come
// with the ID to acknowledge them by.
func WritePlan(w io.Writer, p SchemaPlan) {
	if len(p.Steps) == 0 {
		_, _ = fmt.Fprintln(w, "No schema changes: the models match the migration directory.")
		return
	}
	_, _ = fmt.Fprintf(w, "Schema plan (%s, %d change(s)):\n\n", p.Driver, len(p.Steps))
	for _, s := range p.Steps {
		label := strings.ToUpper(string(s.Severity))
		if s.Severity == SeveritySafe {
			label = "safe"
		}
		ack := ""
		if s.Acknowledged && (s.Severity == SeverityDestructive || s.Severity == SeverityUnsafe) {
			ack = "  (allowed)"
		}
		_, _ = fmt.Fprintf(w, "  %-12s %s%s\n", label, s.ID, ack)
		_, _ = fmt.Fprintf(w, "  %-12s %s\n", "", s.Detail)
		if s.Hint != "" {
			for _, line := range strings.Split(s.Hint, "\n") {
				_, _ = fmt.Fprintf(w, "  %-12s %s\n", "", line)
			}
		}
	}
	if pending := p.Unacknowledged(); len(pending) > 0 {
		_, _ = fmt.Fprintf(w, "\n%d destructive or unsafe change(s) need acknowledgement. Handle the data, then pass --allow for each:\n", len(pending))
		for _, s := range pending {
			_, _ = fmt.Fprintf(w, "  --allow %s\n", s.ID)
		}
	}
}

// defaultTableNames are the tables GORM names models by when they do not
// override TableName.
func defaultTableNames(models []migrations.Model) map[string]bool {
	names := make(map[string]bool, len(models))
	naming := gormschema.NamingStrategy{}
	for _, m := range models {
		names[naming.TableName(m.TypeName)] = true
	}
	return names
}

// alignSchemas gives both realms the same schemas. An empty migration
// directory inspects to no schema at all; the diff should then be tables to
// add, not a schema to create.
func alignSchemas(a, b *schema.Realm) {
	add := func(dst, src *schema.Realm) {
		for _, s := range src.Schemas {
			if _, ok := dst.Schema(s.Name); !ok {
				dst.AddSchemas(schema.New(s.Name).AddAttrs(s.Attrs...))
			}
		}
	}
	add(a, b)
	add(b, a)
}

func evalHCL(driver config.DatabaseDriver, data []byte, realm *schema.Realm) error {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	switch driver {
	case config.DatabaseDriverSQLite:
		return sqlite.EvalHCLBytes(data, realm, nil)
	case config.DatabaseDriverPostgres:
		return postgres.EvalHCLBytes(data, realm, nil)
	case config.DatabaseDriverMySQL:
		return mysql.EvalHCLBytes(data, realm, nil)
	}
	return fmt.Errorf("unsupported driver %q", driver)
}

func differ(driver config.DatabaseDriver) schema.Differ {
	switch driver {
	case config.DatabaseDriverPostgres:
		return postgres.DefaultDiff
	case config.DatabaseDriverMySQL:
		return mysql.DefaultDiff
	default:
		return sqlite.DefaultDiff
	}
}
