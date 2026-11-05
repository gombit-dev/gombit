package migrations

import (
	"path/filepath"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

func TestDropSQLiteSchemaOnlyPreservesReservedPrefix(t *testing.T) {
	cfg := config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "reset.db") + "?cache=shared&_fk=1",
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, ddl := range []string{
		"CREATE TABLE normal_table (id INTEGER PRIMARY KEY)",
		"CREATE TABLE sqlitex_cache (id INTEGER PRIMARY KEY)",
		"CREATE TABLE sqlite3_import (id INTEGER PRIMARY KEY)",
		"CREATE TABLE SqlitEy_cache (id INTEGER PRIMARY KEY)",
		"CREATE TABLE reserved_anchor (id INTEGER PRIMARY KEY AUTOINCREMENT)",
		"CREATE VIEW sqlitex_view AS SELECT id FROM reserved_anchor",
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("fixture %q: %v", ddl, err)
		}
	}
	if err := dropSQLiteSchema(db); err != nil {
		t.Fatalf("dropSQLiteSchema: %v", err)
	}

	var remaining []struct{ Name string }
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type IN ('table','view') ORDER BY name").
		Scan(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Name != "sqlite_sequence" {
		t.Fatalf("remaining SQLite objects = %+v, want only sqlite_sequence", remaining)
	}
}
