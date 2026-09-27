package migrations

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// inspectFakeAtlas answers `atlas schema inspect` with a fixed document (or
// failure) and delegates migrate calls to applyFakeAtlas.
type inspectFakeAtlas struct {
	applyFakeAtlas
	schema     string
	failStderr string
	inspectURL string
}

func (r *inspectFakeAtlas) Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) >= 2 && args[0] == "schema" && args[1] == "inspect" {
		for i, a := range args {
			if a == "--url" && i+1 < len(args) {
				r.inspectURL = args[i+1]
			}
		}
		if r.failStderr != "" {
			_, _ = io.WriteString(stderr, r.failStderr)
			return errors.New("exit status 1")
		}
		_, _ = io.WriteString(stdout, r.schema)
		return nil
	}
	return r.applyFakeAtlas.Run(ctx, dir, name, args, stdout, stderr)
}

func TestInspectDatabaseReadsPendingAndSchema(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")
	writeFile(t, filepath.Join(migrationDir, "20260102000000_add_widget_note.sql"),
		"ALTER TABLE widgets ADD COLUMN note TEXT;")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	runner := &inspectFakeAtlas{applyFakeAtlas: applyFakeAtlas{t: t}, schema: "schema \"main\" {}\n"}
	opts := ApplyOptions{WorkDir: workDir, MigrationDir: "database/migrations", Database: cfg, runner: runner}

	// A database no migration has touched: everything is pending, and reading
	// it does not create the ledger.
	state, err := InspectDatabase(context.Background(), opts)
	if err != nil {
		t.Fatalf("InspectDatabase() before migrate: %v", err)
	}
	if got := versions(state.Pending); got != "20260101000000,20260102000000" || state.LastApplied != "" {
		t.Fatalf("before migrate: pending = %s, last = %q", got, state.LastApplied)
	}
	if string(state.Schema) != runner.schema {
		t.Fatalf("schema = %q, want the atlas schema inspect output", state.Schema)
	}
	if !strings.HasPrefix(runner.inspectURL, "sqlite://") {
		t.Fatalf("schema inspect --url = %q, want the application database", runner.inspectURL)
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable(&Revision{}) {
		t.Fatal("InspectDatabase created framework_migrations")
	}
	_ = db.Close()

	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	state, err = InspectDatabase(context.Background(), opts)
	if err != nil {
		t.Fatalf("InspectDatabase() after migrate: %v", err)
	}
	if len(state.Pending) != 0 || state.LastApplied != "20260102000000" || len(state.Unknown) != 0 {
		t.Fatalf("after migrate: pending = %s, last = %q, unknown = %v", versions(state.Pending), state.LastApplied, state.Unknown)
	}

	// An applied migration deleted from the directory is reported, not
	// silently taken as never applied.
	if err := os.Remove(filepath.Join(migrationDir, "20260102000000_add_widget_note.sql")); err != nil {
		t.Fatal(err)
	}
	state, err = InspectDatabase(context.Background(), opts)
	if err != nil {
		t.Fatalf("InspectDatabase() after a deleted file: %v", err)
	}
	if strings.Join(state.Unknown, ",") != "20260102000000" || state.LastApplied != "20260101000000" || len(state.Pending) != 0 {
		t.Fatalf("after a deleted file: unknown = %v, last = %q, pending = %s", state.Unknown, state.LastApplied, versions(state.Pending))
	}
}

func TestInspectDatabaseReportsAtlasFailure(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "database", "migrations", "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY);")
	runner := &inspectFakeAtlas{applyFakeAtlas: applyFakeAtlas{t: t}, failStderr: "Error: connection refused\n"}
	state, err := InspectDatabase(context.Background(), ApplyOptions{
		WorkDir:  workDir,
		Database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: filepath.Join(workDir, "app.db")},
		runner:   runner,
	})
	if err == nil || !strings.Contains(err.Error(), "connection refused") || !errors.Is(err, ErrSchemaInspect) {
		t.Fatalf("InspectDatabase() error = %v, want the atlas message wrapping ErrSchemaInspect", err)
	}
	// The ledger was read before the schema; the failure does not erase it.
	if versions(state.Pending) != "20260101000000" {
		t.Fatalf("pending after a failed schema read = %s, want the unapplied migration", versions(state.Pending))
	}
}

func versions(files []MigrationFile) string {
	var out []string
	for _, f := range files {
		out = append(out, f.Version)
	}
	return strings.Join(out, ",")
}
