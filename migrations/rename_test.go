package migrations

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

func TestParseRename(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    Rename
		wantErr bool
	}{
		{
			name: "valid",
			spec: "guilds.name:title",
			want: Rename{Table: "guilds", OldColumn: "name", NewColumn: "title"},
		},
		{
			name: "trims spaces",
			spec: "  guilds.name : title ",
			want: Rename{Table: "guilds", OldColumn: "name", NewColumn: "title"},
		},
		{name: "empty", spec: "", wantErr: true},
		{name: "no colon", spec: "guilds.name", wantErr: true},
		{name: "no dot", spec: "name:title", wantErr: true},
		{name: "missing new", spec: "guilds.name:", wantErr: true},
		{name: "missing old", spec: "guilds.:title", wantErr: true},
		{name: "missing table", spec: ".name:title", wantErr: true},
		{name: "same column", spec: "guilds.name:name", wantErr: true},
		{name: "bad identifier", spec: "guilds.na-me:title", wantErr: true},
		{name: "injection attempt", spec: "guilds.name:title; DROP TABLE users", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRename(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRename(%q) error = nil, want error", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRename(%q) error = %v", tt.spec, err)
			}
			if got != tt.want {
				t.Fatalf("ParseRename(%q) = %#v, want %#v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestRenameSQLPerDriver(t *testing.T) {
	renames := []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}}
	tests := []struct {
		driver config.DatabaseDriver
		want   string
	}{
		{config.DatabaseDriverSQLite, "ALTER TABLE `guilds` RENAME COLUMN `name` TO `title`;"},
		{config.DatabaseDriverMySQL, "ALTER TABLE `guilds` RENAME COLUMN `name` TO `title`;"},
		{config.DatabaseDriverPostgres, `ALTER TABLE "guilds" RENAME COLUMN "name" TO "title";`},
	}
	for _, tt := range tests {
		t.Run(string(tt.driver), func(t *testing.T) {
			got := renameSQL(tt.driver, renames)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("renameSQL(%s) = %q, want it to contain %q", tt.driver, got, tt.want)
			}
			// Data-preserving: never a drop or a table rebuild.
			for _, banned := range []string{"DROP COLUMN", "INSERT INTO", "CREATE TABLE"} {
				if strings.Contains(got, banned) {
					t.Fatalf("renameSQL(%s) = %q, must not contain %q", tt.driver, got, banned)
				}
			}
		})
	}
}

func TestRenameSQLMultiple(t *testing.T) {
	got := renameSQL(config.DatabaseDriverPostgres, []Rename{
		{Table: "guilds", OldColumn: "name", NewColumn: "title"},
		{Table: "members", OldColumn: "handle", NewColumn: "username"},
	})
	if strings.Count(got, "RENAME COLUMN") != 2 {
		t.Fatalf("want two RENAME COLUMN statements, got %q", got)
	}
}

func TestNextMigrationVersion(t *testing.T) {
	dir := t.TempDir()
	// Empty dir: a fresh timestamp.
	v, err := nextMigrationVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 14 {
		t.Fatalf("version = %q, want a 14-digit timestamp", v)
	}

	// A far-future migration forces a bump to latest+1 (clock is behind it).
	future := "29990101000000"
	if err := os.WriteFile(filepath.Join(dir, future+"_create_guilds.sql"), []byte("-- x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = nextMigrationVersion(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v != "29990101000001" {
		t.Fatalf("version = %q, want 29990101000001 (latest+1)", v)
	}
}

// hashRecordingRunner records the atlas invocation without touching a dev DB, so
// the rename path can be tested without the atlas binary.
type hashRecordingRunner struct {
	name string
	args []string
	ran  bool
}

func (r *hashRecordingRunner) Run(_ context.Context, _ string, name string, args []string, _ io.Writer, _ io.Writer) error {
	r.ran = true
	r.name = name
	r.args = append([]string(nil), args...)
	return nil
}

func TestMakeMigrationsRenameWritesNativeRenameAndHashes(t *testing.T) {
	workDir := t.TempDir()
	runner := &hashRecordingRunner{}
	stdout := new(strings.Builder)

	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_guild_title",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Renames:      []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}},
		Stdout:       stdout,
		runner:       runner,
	})
	if err != nil {
		t.Fatalf("MakeMigrations() error = %v", err)
	}

	// The migration diff loader (`go run`) must never run for a rename.
	if runner.name != "atlas-test" {
		t.Fatalf("expected atlas hash, ran %q", runner.name)
	}
	if len(runner.args) < 2 || runner.args[0] != "migrate" || runner.args[1] != "hash" {
		t.Fatalf("atlas args = %v, want a migrate hash", runner.args)
	}

	migrationDir := filepath.Join(workDir, "database/migrations")
	entries, err := os.ReadDir(migrationDir)
	if err != nil {
		t.Fatal(err)
	}
	var sqlName string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_rename_guild_title.sql") {
			sqlName = e.Name()
		}
	}
	if sqlName == "" {
		t.Fatalf("no rename migration written; dir = %v", entries)
	}
	// #nosec G304 -- test file under the test temp dir
	sql, err := os.ReadFile(filepath.Join(migrationDir, sqlName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sql), "RENAME COLUMN `name` TO `title`") {
		t.Fatalf("migration SQL = %q, want a native RENAME COLUMN", sql)
	}
	if strings.Contains(string(sql), "DROP COLUMN") || strings.Contains(string(sql), "INSERT INTO") {
		t.Fatalf("migration SQL = %q, must be data-preserving (no drop/rebuild)", sql)
	}
}

func TestMakeMigrationsRenameRejectsModelCombination(t *testing.T) {
	runner := &hashRecordingRunner{}
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      t.TempDir(),
		Name:         "rename_guild_title",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Renames:      []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}},
		Models:       []Model{{ImportPath: "github.com/example/app/internal/guild", TypeName: "Guild"}},
		Stdout:       io.Discard,
		runner:       runner,
	})
	if err == nil {
		t.Fatal("MakeMigrations() error = nil, want rejection of --rename with --model")
	}
	if !strings.Contains(err.Error(), "--rename cannot be combined") {
		t.Fatalf("error = %v, want it to reject the combination", err)
	}
	if runner.ran {
		t.Fatal("atlas must not run when the option combination is rejected")
	}
}

// TestRenameMigrationPreservesDataSQLite is the regression for #299: a field
// rename must keep the column's rows, not drop them. It applies a real
// RENAME COLUMN migration to a real SQLite database (the fake atlas executes the
// SQL) and asserts the pre-existing row survives under the new column.
func TestRenameMigrationPreservesDataSQLite(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_guilds.sql"),
		"CREATE TABLE guilds (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	applyOpts := func() ApplyOptions {
		return ApplyOptions{
			WorkDir:      workDir,
			MigrationDir: "database/migrations",
			AtlasBinary:  "atlas",
			Database:     cfg,
			Stdout:       io.Discard,
			Stderr:       io.Discard,
			runner:       &applyFakeAtlas{t: t},
		}
	}

	if err := Migrate(context.Background(), applyOpts()); err != nil {
		t.Fatalf("Migrate(create) error = %v", err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO guilds (name) VALUES ('Alpha')").Error; err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Generate the rename migration (native RENAME COLUMN).
	if err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_guild_title",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Renames:      []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}},
		Stdout:       io.Discard,
		runner:       &hashRecordingRunner{},
	}); err != nil {
		t.Fatalf("MakeMigrations(rename) error = %v", err)
	}

	if err := Migrate(context.Background(), applyOpts()); err != nil {
		t.Fatalf("Migrate(rename) error = %v", err)
	}

	db, err = database.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var title string
	if err := db.Raw("SELECT title FROM guilds WHERE id = 1").Scan(&title).Error; err != nil {
		t.Fatalf("select title after rename: %v", err)
	}
	if title != "Alpha" {
		t.Fatalf("title = %q after rename, want the row preserved as \"Alpha\" (data loss = #299)", title)
	}
	// The old column is gone (this was a rename, not an add).
	if err := db.Exec("SELECT name FROM guilds").Error; err == nil {
		t.Fatal("old column 'name' still exists; rename did not replace it")
	}
}
