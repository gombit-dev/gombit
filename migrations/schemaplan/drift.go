package schemaplan

import (
	"fmt"
	"sort"

	"ariga.io/atlas/sql/schema"

	"github.com/gombit-dev/gombit/config"
)

// atlasRevisionsSchema is the schema Atlas creates for its revisions table on
// PostgreSQL when --revisions-schema does not pin it elsewhere.
const atlasRevisionsSchema = "atlas_schema_revisions"

// Schema-level drift codes. They exist only in DatabaseDrift: a migration plan
// never creates or drops a schema.
const (
	StepExtraSchema   = "extra_schema"
	StepMissingSchema = "missing_schema"
)

// DatabaseDrift compares the live application database (live, the HCL
// `atlas schema inspect` prints for it) with the schema its applied
// migrations build (expected, the migration directory inspected at the last
// applied version). Each step is something the database has that the
// migrations do not say, or the reverse: a column added by hand, an index
// dropped outside a migration, a table moved to another schema. No steps
// means no drift.
//
// Two things are set aside, and nothing else. The ignore tables (Gombit's and
// Atlas's bookkeeping) are dropped by name, along with Atlas's own revisions
// schema once it holds nothing else. On MySQL, where a schema is the database
// itself, the one schema on each side is compared as one: the dev database the
// migrations are replayed on is named "dev", the live one after the
// application database, and its charset and collation are the server's.
func DatabaseDrift(driver config.DatabaseDriver, expected, live []byte, ignore []string) ([]PlanStep, error) {
	want, got := &schema.Realm{}, &schema.Realm{}
	if err := evalHCL(driver, expected, want); err != nil {
		return nil, fmt.Errorf("schemaplan: read the migrations' schema: %w", err)
	}
	if err := evalHCL(driver, live, got); err != nil {
		return nil, fmt.Errorf("schemaplan: read the database schema: %w", err)
	}
	skip := map[string]bool{}
	for _, t := range ignore {
		skip[t] = true
	}
	kept := got.Schemas[:0]
	for _, s := range got.Schemas {
		tables := s.Tables[:0]
		for _, t := range s.Tables {
			if !skip[t.Name] {
				tables = append(tables, t)
			}
		}
		s.Tables = tables
		if s.Name == atlasRevisionsSchema && len(s.Tables) == 0 && len(s.Views) == 0 && len(s.Funcs) == 0 && len(s.Procs) == 0 {
			continue
		}
		kept = append(kept, s)
	}
	got.Schemas = kept
	if driver == config.DatabaseDriverMySQL && len(want.Schemas) == 1 && len(got.Schemas) == 1 {
		got.Schemas[0].Name = want.Schemas[0].Name
		got.Schemas[0].Attrs = want.Schemas[0].Attrs
	}
	// A database no migration has touched yet: the migrations build nothing,
	// so the live schemas must be empty, not absent.
	if len(want.Schemas) == 0 {
		for _, s := range got.Schemas {
			want.AddSchemas(schema.New(s.Name).AddAttrs(s.Attrs...))
		}
	}

	names := map[string]bool{}
	for _, s := range want.Schemas {
		names[s.Name] = true
	}
	for _, s := range got.Schemas {
		names[s.Name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	var steps []PlanStep
	for _, name := range ordered {
		w, inWant := want.Schema(name)
		g, inGot := got.Schema(name)
		switch {
		case !inWant:
			steps = append(steps, newStep(StepExtraSchema, SeverityUnsafe, name, "", fmt.Sprintf("The database has schema %s, which the migrations do not create.", name)))
			w = schema.New(name).AddAttrs(g.Attrs...)
		case !inGot:
			steps = append(steps, newStep(StepMissingSchema, SeverityUnsafe, name, "", fmt.Sprintf("The migrations create schema %s, which the database does not have.", name)))
			g = schema.New(name).AddAttrs(w.Attrs...)
		}
		changes, err := differ(driver).SchemaDiff(w, g)
		if err != nil {
			return nil, fmt.Errorf("schemaplan: diff schema %s: %w", name, err)
		}
		for _, s := range classifyChanges(driver, changes) {
			if len(ordered) > 1 {
				s.Detail += fmt.Sprintf(" (schema %s)", name)
			}
			steps = append(steps, s)
		}
	}
	return steps, nil
}
