//go:build conformance

package conformance_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
)

// TestDatabaseCheck is `gombit db check`'s database-schema layer on each
// driver: a migrated database matches its migrations, with Gombit's and
// Atlas's bookkeeping (on PostgreSQL, Atlas's own revisions schema) set
// aside, and a column added outside a migration is reported (#313).
func TestDatabaseCheck(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir := migrations.DirOptions{WorkDir: h.workDir, Driver: h.cfg.Driver, MigrationDir: "database/migrations", AtlasBinary: h.atlasBin}

	drift := func() []string {
		t.Helper()
		state, err := migrations.InspectDatabase(ctx, h.opts)
		if err != nil {
			t.Fatalf("InspectDatabase() error = %v", err)
		}
		if len(state.Pending) != 0 || len(state.Unknown) != 0 || state.LastApplied == "" {
			t.Fatalf("state = pending %v, unknown %v, last %q; want the one migration applied", state.Pending, state.Unknown, state.LastApplied)
		}
		expected, err := migrations.InspectDirAt(ctx, dir, state.LastApplied)
		if err != nil {
			t.Fatalf("InspectDirAt() error = %v", err)
		}
		steps, err := schemaplan.DatabaseDrift(h.cfg.Driver, expected, state.Schema, migrations.BookkeepingTables)
		if err != nil {
			t.Fatalf("DatabaseDrift() error = %v", err)
		}
		var ids []string
		for _, s := range steps {
			ids = append(ids, s.ID)
		}
		return ids
	}

	if ids := drift(); len(ids) != 0 {
		t.Fatalf("a freshly migrated database drifts: %v", ids)
	}

	db := h.openDB()
	if err := db.Exec("ALTER TABLE items ADD COLUMN hand_note varchar(20)").Error; err != nil {
		t.Fatalf("add a column by hand: %v", err)
	}
	if ids := drift(); strings.Join(ids, ",") != "add_column:items.hand_note" {
		t.Fatalf("drift after a hand-added column = %v, want [add_column:items.hand_note]", ids)
	}
	if err := db.Exec("ALTER TABLE items DROP COLUMN hand_note").Error; err != nil {
		t.Fatalf("drop the hand-added column: %v", err)
	}

	if h.cfg.Driver != config.DatabaseDriverPostgres {
		return
	}
	// On PostgreSQL a schema is part of what the migrations build: a table
	// moved out of public is drift, even though the bookkeeping stays there.
	for _, stmt := range []string{"CREATE SCHEMA moved", "ALTER TABLE items SET SCHEMA moved"} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_ = db.Exec("ALTER TABLE moved.items SET SCHEMA public").Error
		_ = db.Exec("DROP SCHEMA IF EXISTS moved").Error
	})
	ids := strings.Join(drift(), ",")
	for _, want := range []string{"extra_schema:moved", "add_table:items", "drop_table:items"} {
		if !strings.Contains(ids, want) {
			t.Fatalf("drift after moving items to another schema = %s, want %s", ids, want)
		}
	}
}
