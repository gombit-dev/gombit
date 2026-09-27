package schemaplan

import (
	"fmt"

	"ariga.io/atlas/sql/schema"

	"github.com/gombit-dev/gombit/config"
)

// DatabaseDrift compares the live application database (live, the HCL
// `atlas schema inspect` prints for it) with the schema its applied
// migrations build (expected, the migration directory inspected at the last
// applied version). Each step is something the database has that the
// migrations do not say, or the reverse: a column added by hand, an index
// dropped outside a migration. No steps means no drift.
//
// ignore lists tables the comparison skips (Gombit's and Atlas's bookkeeping
// tables). When both sides hold a single schema they are compared as one: the
// dev database names it after itself (MySQL "dev"), the live one after the
// application database.
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
	for _, s := range got.Schemas {
		kept := s.Tables[:0]
		for _, t := range s.Tables {
			if !skip[t.Name] {
				kept = append(kept, t)
			}
		}
		s.Tables = kept
	}
	// Schemas that only held bookkeeping tables (PostgreSQL may keep Atlas's
	// revisions in a schema of their own) are not part of the comparison.
	kept := got.Schemas[:0]
	for _, s := range got.Schemas {
		if len(s.Tables) > 0 || len(got.Schemas) == 1 {
			kept = append(kept, s)
		}
	}
	got.Schemas = kept
	if len(want.Schemas) == 1 && len(got.Schemas) == 1 {
		got.Schemas[0].Name = want.Schemas[0].Name
		// Schema-level attributes (charset, comment) belong to the database
		// server, not to the migrations.
		got.Schemas[0].Attrs = want.Schemas[0].Attrs
	}
	alignSchemas(want, got)
	changes, err := differ(driver).RealmDiff(want, got)
	if err != nil {
		return nil, fmt.Errorf("schemaplan: diff schemas: %w", err)
	}
	return append([]PlanStep{}, classifyChanges(driver, changes)...), nil
}
