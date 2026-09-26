package migrations

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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
			got := renameSQL(tt.driver, nil, renames)
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
	got := renameSQL(config.DatabaseDriverPostgres, nil, []Rename{
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

// failingHashRunner simulates `atlas migrate hash` failing after partially
// rewriting atlas.sum (the worst case), then returning a non-zero exit.
type failingHashRunner struct{ dir string }

func (r *failingHashRunner) Run(_ context.Context, _ string, _ string, _ []string, _ io.Writer, _ io.Writer) error {
	_ = os.WriteFile(filepath.Join(r.dir, "atlas.sum"), []byte("partial garbage\n"), 0o600)
	return errors.New("exit status 1")
}

func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue // downs/: the snapshot is the files Atlas hashes
		}
		// #nosec G304 -- test file under the test temp dir
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func TestMakeMigrationsRenameHashFailureLeavesDirUnchanged(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_guilds.sql"),
		"CREATE TABLE guilds (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")
	writeFile(t, filepath.Join(migrationDir, "atlas.sum"), "h1:original\n20260101000000_create_guilds.sql h1:abc\n")
	before := dirSnapshot(t, migrationDir)

	opts := Options{
		WorkDir:      workDir,
		Name:         "rename_guild_title",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Renames:      []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}},
		Stdout:       io.Discard,
		runner:       &failingHashRunner{dir: migrationDir},
	}
	err := MakeMigrations(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "atlas migrate hash") {
		t.Fatalf("MakeMigrations() error = %v, want the atlas migrate hash failure", err)
	}
	after := dirSnapshot(t, migrationDir)
	if len(after) != len(before) {
		t.Fatalf("migration dir after failed hash = %v, want it unchanged from %v", keys(after), keys(before))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s after failed hash = %q, want it restored to %q", name, after[name], content)
		}
	}

	// Rerunning once Atlas works must produce exactly one rename migration, not
	// a second RENAME COLUMN next to an orphaned first one.
	opts.runner = &hashRecordingRunner{}
	if err := MakeMigrations(context.Background(), opts); err != nil {
		t.Fatalf("rerun MakeMigrations() error = %v", err)
	}
	renames := 0
	for name := range dirSnapshot(t, migrationDir) {
		if strings.HasSuffix(name, "_rename_guild_title.sql") {
			renames++
		}
	}
	if renames != 1 {
		t.Fatalf("rename migrations after rerun = %d, want exactly 1", renames)
	}
}

func TestMakeMigrationsRenameHashFailureRemovesCreatedDir(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_guild_title",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Renames:      []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}},
		Stdout:       io.Discard,
		runner:       &failingHashRunner{dir: migrationDir},
	})
	if err == nil {
		t.Fatal("MakeMigrations() error = nil, want the hash failure")
	}
	if _, statErr := os.Stat(migrationDir); !os.IsNotExist(statErr) {
		t.Fatalf("migration dir created by the failed run must be removed; stat err = %v", statErr)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseTableRename(t *testing.T) {
	got, err := ParseTableRename(" products : items ")
	if err != nil || got != (TableRename{Old: "products", New: "items"}) {
		t.Fatalf("ParseTableRename() = %+v, %v", got, err)
	}
	for _, bad := range []string{"products", "products:", ":items", "products:products", "prod-ucts:items", "products:it;ems"} {
		if _, err := ParseTableRename(bad); err == nil {
			t.Errorf("ParseTableRename(%q) = nil error, want rejection", bad)
		}
	}
}

func TestParseRenameEqualsForm(t *testing.T) {
	got, err := ParseRename("products.name=products.title")
	if err != nil || got != (Rename{Table: "products", OldColumn: "name", NewColumn: "title"}) {
		t.Fatalf("ParseRename(=) = %+v, %v", got, err)
	}
	if _, err := ParseRename("products.name=items.title"); err == nil || !strings.Contains(err.Error(), "--rename-table products:items") {
		t.Fatalf("ParseRename across tables error = %v, want the --rename-table pointer", err)
	}
}

func TestValidateRenameSet(t *testing.T) {
	tables := []TableRename{{Old: "products", New: "items"}}
	if err := validateRenameSet(tables, []Rename{{Table: "items", OldColumn: "name", NewColumn: "title"}}); err != nil {
		t.Fatalf("column rename on the new table name: %v", err)
	}
	if err := validateRenameSet(tables, []Rename{{Table: "products", OldColumn: "name", NewColumn: "title"}}); err == nil || !strings.Contains(err.Error(), "use its new name") {
		t.Fatalf("column rename on the old table name error = %v, want rejection", err)
	}
	if err := validateRenameSet([]TableRename{{Old: "a", New: "b"}, {Old: "a", New: "c"}}, nil); err == nil {
		t.Fatal("renaming one table twice must be rejected")
	}
	if err := validateRenameSet([]TableRename{{Old: "a", New: "c"}, {Old: "b", New: "c"}}, nil); err == nil {
		t.Fatal("renaming two tables to one name must be rejected")
	}
}

func TestRenameSQLTablesBeforeColumns(t *testing.T) {
	got := renameSQL(config.DatabaseDriverPostgres, []TableRename{{Old: "products", New: "items"}}, []Rename{{Table: "items", OldColumn: "name", NewColumn: "title"}})
	table := strings.Index(got, `ALTER TABLE "products" RENAME TO "items";`)
	column := strings.Index(got, `ALTER TABLE "items" RENAME COLUMN "name" TO "title";`)
	if table < 0 || column < 0 || table > column {
		t.Fatalf("renameSQL() = %q, want the table rename before the column rename", got)
	}
	if lite := renameSQL(config.DatabaseDriverSQLite, []TableRename{{Old: "products", New: "items"}}, nil); !strings.Contains(lite, "ALTER TABLE `products` RENAME TO `items`;") {
		t.Fatalf("renameSQL(sqlite) = %q", lite)
	}
}

func TestMakeMigrationsTableRenameSwapsRegistry(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	product := Model{ImportPath: "example.com/app/internal/product", TypeName: "Product"}
	item := Model{ImportPath: "example.com/app/internal/item", TypeName: "Item"}
	order := Model{ImportPath: "example.com/app/internal/order", TypeName: "Order"}
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(migrationDir, []Model{product, order}); err != nil {
		t.Fatal(err)
	}
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		TableRenames: []TableRename{{Old: "products", New: "items"}},
		Models:       []Model{item},
		ForgetModels: []Model{product},
		Stdout:       io.Discard,
		runner:       &hashRecordingRunner{},
	})
	if err != nil {
		t.Fatalf("MakeMigrations(--rename-table) error = %v", err)
	}
	got, err := LoadRegistry(migrationDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !slices.Contains(got, item) || !slices.Contains(got, order) || slices.Contains(got, product) {
		t.Fatalf("registry = %v, want Item swapped in for Product and Order kept", got)
	}
}

func TestMakeMigrationsTableRenameRejectsUntrackedForget(t *testing.T) {
	runner := &hashRecordingRunner{}
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      t.TempDir(),
		Name:         "rename_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		TableRenames: []TableRename{{Old: "products", New: "items"}},
		ForgetModels: []Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}},
		Stdout:       io.Discard,
		runner:       runner,
	})
	if err == nil || !strings.Contains(err.Error(), "not tracked") {
		t.Fatalf("MakeMigrations() error = %v, want the untracked --forget-model rejection", err)
	}
	if runner.ran {
		t.Fatal("atlas must not run when the registry swap is rejected")
	}
}

func TestMakeMigrationsTableRenameHashFailureRestoresRegistry(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_products.sql"), "CREATE TABLE products (id INTEGER PRIMARY KEY);")
	writeFile(t, filepath.Join(migrationDir, "atlas.sum"), "h1:original\n20260101000000_create_products.sql h1:abc\n")
	product := Model{ImportPath: "example.com/app/internal/product", TypeName: "Product"}
	if err := SaveRegistry(migrationDir, []Model{product}); err != nil {
		t.Fatal(err)
	}
	before := dirSnapshot(t, migrationDir)
	err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		TableRenames: []TableRename{{Old: "products", New: "items"}},
		Models:       []Model{{ImportPath: "example.com/app/internal/item", TypeName: "Item"}},
		ForgetModels: []Model{product},
		Stdout:       io.Discard,
		runner:       &failingHashRunner{dir: migrationDir},
	})
	if err == nil {
		t.Fatal("MakeMigrations() error = nil, want the hash failure")
	}
	after := dirSnapshot(t, migrationDir)
	if len(after) != len(before) {
		t.Fatalf("migration dir after failed hash = %v, want %v", keys(after), keys(before))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s changed after a failed hash:\n%s\nwant\n%s", name, after[name], content)
		}
	}
}

func TestValidateRenameSetRejectsChains(t *testing.T) {
	for _, tables := range [][]TableRename{
		{{Old: "a", New: "b"}, {Old: "b", New: "c"}},
		{{Old: "a", New: "b"}, {Old: "b", New: "a"}},
	} {
		if err := validateRenameSet(tables, nil); err == nil || !strings.Contains(err.Error(), "separate migrations") {
			t.Errorf("validateRenameSet(%v) = %v, want the chain rejection", tables, err)
		}
	}
}

func TestRenameDownSQLIsTheInverse(t *testing.T) {
	got := renameDownSQL(config.DatabaseDriverSQLite,
		[]TableRename{{Old: "products", New: "items"}},
		[]Rename{{Table: "items", OldColumn: "name", NewColumn: "title"}, {Table: "items", OldColumn: "cost", NewColumn: "price"}})
	want := "ALTER TABLE `items` RENAME COLUMN `price` TO `cost`;\n" +
		"ALTER TABLE `items` RENAME COLUMN `title` TO `name`;\n" +
		"ALTER TABLE `items` RENAME TO `products`;\n"
	if got != want {
		t.Fatalf("renameDownSQL() =\n%s\nwant\n%s", got, want)
	}
}

// TestTableRenameMigratesAndRollsBackSQLite applies a table + column rename to
// a real SQLite database (the fake Atlas executes the SQL), then rolls it back
// with the generated down, and checks the row survives both ways.
func TestTableRenameMigratesAndRollsBackSQLite(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_products.sql"),
		"CREATE TABLE products (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	apply := ApplyOptions{WorkDir: workDir, MigrationDir: "database/migrations", AtlasBinary: "atlas", Database: cfg, Stdout: io.Discard, Stderr: io.Discard, runner: &applyFakeAtlas{t: t}}
	if err := Migrate(context.Background(), apply); err != nil {
		t.Fatalf("Migrate(create) error = %v", err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Exec("INSERT INTO products (name) VALUES ('Alpha')").Error; err != nil {
		t.Fatal(err)
	}

	if err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_products",
		Driver:       config.DatabaseDriverSQLite,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		TableRenames: []TableRename{{Old: "products", New: "items"}},
		Renames:      []Rename{{Table: "items", OldColumn: "name", NewColumn: "title"}},
		Stdout:       io.Discard,
		runner:       &hashRecordingRunner{},
	}); err != nil {
		t.Fatalf("MakeMigrations(--rename-table) error = %v", err)
	}
	if err := Migrate(context.Background(), apply); err != nil {
		t.Fatalf("Migrate(rename) error = %v", err)
	}
	var title string
	if err := db.Raw("SELECT title FROM items").Row().Scan(&title); err != nil || title != "Alpha" {
		t.Fatalf("items.title = %q (%v), want the row kept", title, err)
	}

	if err := Rollback(context.Background(), apply); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	var name string
	if err := db.Raw("SELECT name FROM products").Row().Scan(&name); err != nil || name != "Alpha" {
		t.Fatalf("products.name after rollback = %q (%v), want the row back under the old names", name, err)
	}
}
