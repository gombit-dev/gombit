package migrations

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

func TestSeedAppliesOrderedSQLFiles(t *testing.T) {
	workDir := t.TempDir()
	seedDir := filepath.Join(workDir, "database", "seeds")
	if err := os.MkdirAll(seedDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Exec("CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)").Error; err != nil {
		t.Fatalf("create widgets: %v", err)
	}

	writeFile(t, filepath.Join(seedDir, "02_second.sql"), `INSERT INTO widgets (id, name) VALUES (2, 'second');`)
	writeFile(t, filepath.Join(seedDir, "01_first.sql"), `INSERT INTO widgets (id, name) VALUES (1, 'first');`)
	writeFile(t, filepath.Join(seedDir, "notes.txt"), "not sql")
	if err := os.MkdirAll(filepath.Join(seedDir, "nested"), 0o750); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err = Seed(context.Background(), SeedOptions{
		WorkDir:  workDir,
		SeedDir:  "database/seeds",
		Database: cfg,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	if err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Applied 2 seed file(s)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "skipping non-SQL seed file notes.txt") {
		t.Fatalf("stderr = %q, want skip warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "skipping nested seed directory nested") {
		t.Fatalf("stderr = %q, want nested dir warning", stderr.String())
	}

	var names []string
	if err := db.Raw("SELECT name FROM widgets ORDER BY id").Scan(&names).Error; err != nil {
		t.Fatalf("select widgets: %v", err)
	}
	if len(names) != 2 || names[0] != "first" || names[1] != "second" {
		t.Fatalf("names = %#v, want [first second]", names)
	}
}

func TestSeedMissingDirIsNoop(t *testing.T) {
	workDir := t.TempDir()
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	stdout := new(bytes.Buffer)
	err := Seed(context.Background(), SeedOptions{
		WorkDir:  workDir,
		SeedDir:  "database/seeds",
		Database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn},
		Stdout:   stdout,
	})
	if err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "No seed files") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSeedAppliesMultiStatementFile(t *testing.T) {
	workDir := t.TempDir()
	seedDir := filepath.Join(workDir, "database", "seeds")
	if err := os.MkdirAll(seedDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Exec("CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL)").Error; err != nil {
		t.Fatalf("create widgets: %v", err)
	}

	writeFile(t, filepath.Join(seedDir, "01_multi.sql"), `
-- seed two rows in one file
INSERT INTO widgets (id, name) VALUES (1, 'alpha');
INSERT INTO widgets (id, name) VALUES (2, 'semi;colon');
INSERT INTO widgets (id, name) VALUES (3, 'gamma');
`)

	if err := Seed(context.Background(), SeedOptions{
		WorkDir:  workDir,
		SeedDir:  "database/seeds",
		Database: cfg,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	var names []string
	if err := db.Raw("SELECT name FROM widgets ORDER BY id").Scan(&names).Error; err != nil {
		t.Fatalf("select widgets: %v", err)
	}
	if len(names) != 3 || names[0] != "alpha" || names[1] != "semi;colon" || names[2] != "gamma" {
		t.Fatalf("names = %#v, want [alpha semi;colon gamma]", names)
	}
}

func TestSplitSQLStatements(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "two inserts",
			sql:  "INSERT INTO t VALUES (1); INSERT INTO t VALUES (2);",
			want: []string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)"},
		},
		{
			name: "semicolon inside string",
			sql:  "INSERT INTO t VALUES ('a;b'); INSERT INTO t VALUES (2)",
			want: []string{"INSERT INTO t VALUES ('a;b')", "INSERT INTO t VALUES (2)"},
		},
		{
			name: "escaped quote",
			sql:  "INSERT INTO t VALUES ('it''s;ok');",
			want: []string{"INSERT INTO t VALUES ('it''s;ok')"},
		},
		{
			name: "line comment with semicolon",
			sql:  "INSERT INTO t VALUES (1); -- skip; me\nINSERT INTO t VALUES (2);",
			want: []string{"INSERT INTO t VALUES (1)", "-- skip; me\nINSERT INTO t VALUES (2)"},
		},
		{
			name: "comment only",
			sql:  "-- just a comment\n/* also comment */",
			want: nil,
		},
		{
			name: "trigger body with inner semicolons",
			sql: "CREATE TRIGGER orders_audit AFTER INSERT ON orders\nBEGIN\n" +
				"  INSERT INTO audit (order_id) VALUES (NEW.id);\n" +
				"  UPDATE audit SET order_id = CASE WHEN 1 THEN 2 END;\n" +
				"END;\nDROP TRIGGER orders_audit;",
			want: []string{
				"CREATE TRIGGER orders_audit AFTER INSERT ON orders\nBEGIN\n" +
					"  INSERT INTO audit (order_id) VALUES (NEW.id);\n" +
					"  UPDATE audit SET order_id = CASE WHEN 1 THEN 2 END;\n" +
					"END",
				"DROP TRIGGER orders_audit",
			},
		},
		{
			name: "procedure body with IF blocks",
			sql: "CREATE PROCEDURE p() BEGIN IF x THEN SET y = 1; END IF; " +
				"UPDATE t SET v = IF(x, 1, 2); END;\nSELECT 1;",
			want: []string{
				"CREATE PROCEDURE p() BEGIN IF x THEN SET y = 1; END IF; " +
					"UPDATE t SET v = IF(x, 1, 2); END",
				"SELECT 1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitSQLStatements(tt.sql)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d (%#v), want %d (%#v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("stmt[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDropSchemaRemovesTables(t *testing.T) {
	workDir := t.TempDir()
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Exec("CREATE TABLE widgets (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatalf("create widgets: %v", err)
	}
	if err := ensureRevisionsTable(db.DB); err != nil {
		t.Fatalf("ensureRevisionsTable: %v", err)
	}
	if err := db.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  version varchar(255) PRIMARY KEY,
  description text,
  type varchar(255),
  applied_at datetime
)`, atlasRevisionsTable)).Error; err != nil {
		t.Fatalf("create atlas revisions: %v", err)
	}

	stdout := new(bytes.Buffer)
	if err := DropSchema(context.Background(), ApplyOptions{
		Database: cfg,
		Stdout:   stdout,
	}); err != nil {
		t.Fatalf("DropSchema() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Dropped database schema") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if db.Migrator().HasTable("widgets") {
		t.Fatal("widgets should be gone after DropSchema")
	}
	if db.Migrator().HasTable(revisionsTable) {
		t.Fatal("framework_migrations should be gone after DropSchema")
	}
	if db.Migrator().HasTable(atlasRevisionsTable) {
		t.Fatal("atlas_schema_revisions should be gone after DropSchema")
	}
}

func TestResetRoundTrip(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	seedDir := filepath.Join(workDir, "database", "seeds")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}
	if err := os.MkdirAll(seedDir, 0o750); err != nil {
		t.Fatalf("mkdir seeds: %v", err)
	}

	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")
	writeFile(t, filepath.Join(migrationDir, "downs", "20260101000000_create_widgets.down.sql"),
		"DROP TABLE widgets;")
	writeFile(t, filepath.Join(seedDir, "01_widgets.sql"),
		"INSERT INTO widgets (id, name) VALUES (1, 'seeded');")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	runner := &applyFakeAtlas{t: t}
	stdout := new(bytes.Buffer)
	opts := ResetOptions{
		ApplyOptions: ApplyOptions{
			WorkDir:      workDir,
			MigrationDir: "database/migrations",
			Database:     cfg,
			Stdout:       stdout,
			Stderr:       io.Discard,
			runner:       runner,
			now:          func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
		},
		SeedDir: "database/seeds",
	}

	if err := Reset(context.Background(), opts); err != nil {
		t.Fatalf("Reset() first error = %v", err)
	}

	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets after reset")
	}
	var name string
	if err := db.Raw("SELECT name FROM widgets WHERE id = 1").Scan(&name).Error; err != nil {
		t.Fatalf("select seeded row: %v", err)
	}
	if name != "seeded" {
		t.Fatalf("name = %q, want seeded", name)
	}

	stdout.Reset()
	if err := Reset(context.Background(), opts); err != nil {
		t.Fatalf("Reset() second error = %v", err)
	}
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets after second reset")
	}
	var count int64
	if err := db.Table("widgets").Count(&count).Error; err != nil {
		t.Fatalf("count widgets: %v", err)
	}
	if count != 1 {
		t.Fatalf("widgets count = %d, want 1 after re-seed", count)
	}
	if !strings.Contains(stdout.String(), "Dropped database schema") {
		t.Fatalf("stdout = %q, want drop message", stdout.String())
	}
}

func TestResetRefusesProductionWithoutForce(t *testing.T) {
	workDir := t.TempDir()
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	err := Reset(context.Background(), ResetOptions{
		ApplyOptions: ApplyOptions{
			WorkDir:  workDir,
			Database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn},
			Stdout:   io.Discard,
			Stderr:   io.Discard,
			runner:   &applyFakeAtlas{t: t},
		},
		Environment: config.EnvironmentProduction,
		Force:       false,
	})
	if err == nil {
		t.Fatal("Reset() error = nil, want production refusal")
	}
	if !strings.Contains(err.Error(), "refuse reset in production without --force") {
		t.Fatalf("Reset() error = %q", err)
	}
}

func TestResetAllowsProductionWithForce(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY);")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	err := Reset(context.Background(), ResetOptions{
		ApplyOptions: ApplyOptions{
			WorkDir:      workDir,
			MigrationDir: "database/migrations",
			Database:     cfg,
			Stdout:       io.Discard,
			Stderr:       io.Discard,
			runner:       &applyFakeAtlas{t: t},
			now:          func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
		},
		Environment: config.EnvironmentProduction,
		Force:       true,
	})
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets after forced production reset")
	}
}
