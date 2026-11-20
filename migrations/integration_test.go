//go:build integration

package migrations

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

var (
	postgresDSN = flag.String("migrations.postgres-dsn", "", "PostgreSQL DSN for migration integration tests")
	mysqlDSN    = flag.String("migrations.mysql-dsn", "", "MySQL DSN for migration integration tests")
)

func TestMigrateRollbackStatusPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -migrations.postgres-dsn to run Postgres migration integration tests")
	}
	runDriverRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	}, `
CREATE TABLE IF NOT EXISTS widgets (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL
);`,
		"ALTER TABLE widgets DROP COLUMN name;\nDROP TABLE IF EXISTS widgets;")
}

func TestMigratePostgresKeepsAtlasRevisionsPerSearchPathSchema(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -migrations.postgres-dsn to run Postgres migration integration tests")
	}

	adminCfg := config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: *postgresDSN}
	admin, err := database.Open(adminCfg)
	if err != nil {
		t.Fatalf("database.Open() admin error = %v", err)
	}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	schemas := []string{"gombit_482_a_" + suffix, "gombit_482_b_" + suffix}
	defer func() {
		for _, schema := range schemas {
			if err := admin.Exec("DROP SCHEMA IF EXISTS " + quotePostgresIdentifier(schema) + " CASCADE").Error; err != nil {
				t.Errorf("drop schema %s: %v", schema, err)
			}
		}
		_ = admin.Close()
	}()
	for _, schema := range schemas {
		if err := admin.Exec("CREATE SCHEMA " + quotePostgresIdentifier(schema)).Error; err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
	}

	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(filepath.Join(migrationDir, downSubdir), 0o750); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"), "CREATE TABLE widgets (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL);")
	writeFile(t, filepath.Join(migrationDir, downSubdir, "20260101000000_create_widgets.down.sql"), "DROP TABLE widgets;")

	applyForSchema := func(schema string) (ApplyOptions, *sqlApplyRunner, *bytes.Buffer) {
		t.Helper()
		dsn, err := postgresDSNWithSearchPath(*postgresDSN, schema)
		if err != nil {
			t.Fatalf("set search_path for %s: %v", schema, err)
		}
		cfg := config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: dsn}
		runner := &sqlApplyRunner{t: t, cfg: cfg}
		stdout := new(bytes.Buffer)
		return ApplyOptions{
			WorkDir:      workDir,
			MigrationDir: "database/migrations",
			Database:     cfg,
			Stdout:       stdout,
			Stderr:       io.Discard,
			runner:       runner,
			now:          func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
		}, runner, stdout
	}

	first, firstRunner, _ := applyForSchema(schemas[0])
	if err := Migrate(context.Background(), first); err != nil {
		t.Fatalf("Migrate(%s): %v", schemas[0], err)
	}
	second, secondRunner, _ := applyForSchema(schemas[1])
	if err := Migrate(context.Background(), second); err != nil {
		t.Fatalf("Migrate(%s): %v", schemas[1], err)
	}
	if err := Migrate(context.Background(), second); err != nil {
		t.Fatalf("second Migrate(%s): %v", schemas[1], err)
	}
	if len(secondRunner.revisionsSchemas) != 1 {
		t.Fatalf("second Migrate(%s) invoked Atlas again; revisions schemas = %v", schemas[1], secondRunner.revisionsSchemas)
	}

	for _, tc := range []struct {
		schema string
		runner *sqlApplyRunner
	}{
		{schema: schemas[0], runner: firstRunner},
		{schema: schemas[1], runner: secondRunner},
	} {
		cfgDSN, err := postgresDSNWithSearchPath(*postgresDSN, tc.schema)
		if err != nil {
			t.Fatalf("set search_path for %s: %v", tc.schema, err)
		}
		db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: cfgDSN})
		if err != nil {
			t.Fatalf("open %s: %v", tc.schema, err)
		}
		if !db.Migrator().HasTable("widgets") {
			t.Errorf("widgets table missing from %s after migrate", tc.schema)
		}
		versions, err := listAtlasVersions(db.DB, tc.schema)
		_ = db.Close()
		if err != nil {
			t.Fatalf("list Atlas versions in %s: %v", tc.schema, err)
		}
		if _, ok := versions["20260101000000"]; !ok {
			t.Errorf("Atlas revisions in %s = %v, want the migration version", tc.schema, versions)
		}
		if len(tc.runner.revisionsSchemas) == 0 || tc.runner.revisionsSchemas[0] != tc.schema {
			t.Errorf("Atlas revisions schemas for %s = %v, want %s", tc.schema, tc.runner.revisionsSchemas, tc.schema)
		}
	}

	status, statusRunner, statusOut := applyForSchema(schemas[1])
	if err := Status(context.Background(), status); err != nil {
		t.Fatalf("Status(%s): %v", schemas[1], err)
	}
	if !strings.Contains(statusOut.String(), "applied") || !strings.Contains(statusOut.String(), "20260101000000") {
		t.Errorf("Status(%s) = %q, want applied migration", schemas[1], statusOut.String())
	}
	if len(statusRunner.revisionsSchemas) != 1 || statusRunner.revisionsSchemas[0] != schemas[1] {
		t.Errorf("Atlas status revisions schemas = %v, want [%s]", statusRunner.revisionsSchemas, schemas[1])
	}

	if err := Rollback(context.Background(), second); err != nil {
		t.Fatalf("Rollback(%s): %v", schemas[1], err)
	}
	for _, tc := range []struct {
		schema       string
		wantTable    bool
		wantRevision bool
	}{{schema: schemas[0], wantTable: true, wantRevision: true}, {schema: schemas[1], wantTable: false, wantRevision: false}} {
		dsn, err := postgresDSNWithSearchPath(*postgresDSN, tc.schema)
		if err != nil {
			t.Fatalf("set search_path for %s: %v", tc.schema, err)
		}
		db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: dsn})
		if err != nil {
			t.Fatalf("open %s after rollback: %v", tc.schema, err)
		}
		gotTable := db.Migrator().HasTable("widgets")
		versions, err := listAtlasVersions(db.DB, tc.schema)
		_ = db.Close()
		if err != nil {
			t.Fatalf("list Atlas versions in %s after rollback: %v", tc.schema, err)
		}
		_, gotRevision := versions["20260101000000"]
		if gotTable != tc.wantTable || gotRevision != tc.wantRevision {
			t.Errorf("after tenant rollback, %s widgets=%v AtlasRevision=%v, want widgets=%v AtlasRevision=%v",
				tc.schema, gotTable, gotRevision, tc.wantTable, tc.wantRevision)
		}
	}

	if err := Migrate(context.Background(), second); err != nil {
		t.Fatalf("re-Migrate(%s) after rollback: %v", schemas[1], err)
	}
	dsn, err := postgresDSNWithSearchPath(*postgresDSN, schemas[1])
	if err != nil {
		t.Fatalf("set search_path for %s: %v", schemas[1], err)
	}
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: dsn})
	if err != nil {
		t.Fatalf("reopen %s after re-migrate: %v", schemas[1], err)
	}
	if !db.Migrator().HasTable("widgets") {
		t.Error("widgets table missing after re-migrate")
	}
	versions, err := listAtlasVersions(db.DB, schemas[1])
	_ = db.Close()
	if err != nil {
		t.Fatalf("list Atlas versions in %s after re-migrate: %v", schemas[1], err)
	}
	if _, ok := versions["20260101000000"]; !ok {
		t.Errorf("Atlas revisions in %s after re-migrate = %v, want the migration version", schemas[1], versions)
	}
}

func postgresDSNWithSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse postgres URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("Postgres integration DSN must be a URL, got scheme %q", u.Scheme)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func TestMigrateRollbackStatusMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -migrations.mysql-dsn to run MySQL migration integration tests")
	}
	runDriverRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	}, `
CREATE TABLE IF NOT EXISTS widgets (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(255) NOT NULL
);`,
		"ALTER TABLE widgets DROP COLUMN name;\nDROP TABLE IF EXISTS widgets;")
}

func TestSeedResetPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -migrations.postgres-dsn to run Postgres migration integration tests")
	}
	runSeedResetRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	}, `
CREATE TABLE IF NOT EXISTS widgets (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL
);`, `INSERT INTO widgets (id, name) VALUES (1, 'seeded');`)
}

func TestSeedResetMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -migrations.mysql-dsn to run MySQL migration integration tests")
	}
	runSeedResetRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	}, `
CREATE TABLE IF NOT EXISTS widgets (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(255) NOT NULL
);`, `INSERT INTO widgets (id, name) VALUES (1, 'seeded');`)
}

func runDriverRoundTrip(t *testing.T, cfg config.DatabaseConfig, upSQL string, downSQL string) {
	t.Helper()

	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"), upSQL)
	writeFile(t, filepath.Join(migrationDir, "downs", "20260101000000_create_widgets.down.sql"), downSQL)

	cleanupDriverDB(t, cfg)
	t.Cleanup(func() { cleanupDriverDB(t, cfg) })

	runner := &sqlApplyRunner{t: t, cfg: cfg}
	stdout := new(bytes.Buffer)
	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: "database/migrations",
		Database:     cfg,
		Stdout:       stdout,
		Stderr:       io.Discard,
		runner:       runner,
		now:          func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
	}

	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets table after migrate")
	}

	stdout.Reset()
	if err := Status(context.Background(), opts); err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "applied") {
		t.Fatalf("status = %q", stdout.String())
	}

	stdout.Reset()
	if err := Rollback(context.Background(), opts); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if db.Migrator().HasTable("widgets") {
		t.Fatal("widgets table should be gone after rollback")
	}
}

func runSeedResetRoundTrip(t *testing.T, cfg config.DatabaseConfig, upSQL string, seedSQL string) {
	t.Helper()

	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	seedDir := filepath.Join(workDir, "database", "seeds")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}
	if err := os.MkdirAll(seedDir, 0o750); err != nil {
		t.Fatalf("mkdir seeds: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"), upSQL)
	writeFile(t, filepath.Join(seedDir, "01_widgets.sql"), seedSQL)

	cleanupDriverDB(t, cfg)
	t.Cleanup(func() { cleanupDriverDB(t, cfg) })

	runner := &sqlApplyRunner{t: t, cfg: cfg}
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
		t.Fatalf("Reset() error = %v", err)
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
	var count int64
	if err := db.Table("widgets").Count(&count).Error; err != nil {
		t.Fatalf("count widgets: %v", err)
	}
	if count != 1 {
		t.Fatalf("widgets count = %d, want 1 after re-seed", count)
	}
}

func cleanupDriverDB(t *testing.T, cfg config.DatabaseConfig) {
	t.Helper()
	if err := DropSchema(context.Background(), ApplyOptions{
		Database: cfg,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("cleanup DropSchema: %v", err)
	}
}

type sqlApplyRunner struct {
	t                *testing.T
	cfg              config.DatabaseConfig
	revisionsSchemas []string
}

func (r *sqlApplyRunner) Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	r.t.Helper()
	schema := atlasRevisionsSchemaArg(args)
	r.revisionsSchemas = append(r.revisionsSchemas, schema)
	if r.cfg.Driver == config.DatabaseDriverPostgres && schema == "" {
		return fmt.Errorf("postgres Atlas args are missing --revisions-schema: %v", args)
	}
	if len(args) < 2 || args[0] != "migrate" {
		return fmt.Errorf("unexpected atlas args: %v", args)
	}
	switch args[1] {
	case "status":
		_, _ = io.WriteString(stdout, "fake atlas status\n")
		return nil
	case "apply":
		return r.apply(dir, args, stdout)
	default:
		return fmt.Errorf("unexpected atlas args: %v", args)
	}
}

func (r *sqlApplyRunner) apply(workDir string, args []string, stdout io.Writer) error {
	_, dirURL, err := parseAtlasURLAndDir(args)
	if err != nil {
		return err
	}
	migrationDir := strings.TrimPrefix(dirURL, "file://")
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(workDir, migrationDir)
	}

	db, err := database.Open(r.cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	relation := atlasRevisionsRelation(db.DB, atlasRevisionsSchemaArg(args))
	if err := db.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  version varchar(255) PRIMARY KEY,
  description text,
  type varchar(255),
  applied_at timestamp NULL
  )`, relation)).Error; err != nil {
		return err
	}

	files, err := ListMigrationFiles(migrationDir)
	if err != nil {
		return err
	}
	var existing []string
	if err := db.Raw(fmt.Sprintf("SELECT version FROM %s", relation)).Scan(&existing).Error; err != nil {
		return err
	}
	applied := make(map[string]struct{}, len(existing))
	for _, version := range existing {
		applied[version] = struct{}{}
	}
	for _, file := range files {
		if _, ok := applied[file.Version]; ok {
			continue
		}
		sqlBytes, err := os.ReadFile(file.UpPath)
		if err != nil {
			return err
		}
		if err := db.Exec(string(sqlBytes)).Error; err != nil {
			return err
		}
		if err := db.Exec(
			fmt.Sprintf("INSERT INTO %s (version, description, type, applied_at) VALUES (?, ?, ?, ?)", relation),
			file.Version,
			file.Name,
			"sql",
			time.Now().UTC(),
		).Error; err != nil {
			return err
		}
	}
	_, _ = io.WriteString(stdout, "fake atlas apply ok\n")
	return nil
}

func atlasRevisionsSchemaArg(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--revisions-schema" {
			return args[i+1]
		}
	}
	return ""
}

func TestRenamePreservesDataPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -migrations.postgres-dsn to run Postgres migration integration tests")
	}
	runRenameRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	}, `
CREATE TABLE IF NOT EXISTS guilds (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL
);`)
}

func TestRenamePreservesDataMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -migrations.mysql-dsn to run MySQL migration integration tests")
	}
	runRenameRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	}, `
CREATE TABLE IF NOT EXISTS guilds (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(255) NOT NULL
);`)
}

// runRenameRoundTrip is the #299 regression on a real driver: create a table with
// a row, rename the column via gombit's native RENAME COLUMN migration, apply it,
// and assert the row survives under the new column.
func runRenameRoundTrip(t *testing.T, cfg config.DatabaseConfig, createSQL string) {
	t.Helper()

	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_guilds.sql"), createSQL)

	cleanupDriverDB(t, cfg)
	t.Cleanup(func() { cleanupDriverDB(t, cfg) })

	applyOpts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: "database/migrations",
		Database:     cfg,
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		runner:       &sqlApplyRunner{t: t, cfg: cfg},
		now:          func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
	}

	if err := Migrate(context.Background(), applyOpts); err != nil {
		t.Fatalf("Migrate(create) error = %v", err)
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	if err := db.Exec("INSERT INTO guilds (name) VALUES ('Alpha')").Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = db.Close()

	if err := MakeMigrations(context.Background(), Options{
		WorkDir:      workDir,
		Name:         "rename_guild_title",
		Driver:       cfg.Driver,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas-test",
		Renames:      []Rename{{Table: "guilds", OldColumn: "name", NewColumn: "title"}},
		Stdout:       io.Discard,
		runner:       &hashRecordingRunner{},
	}); err != nil {
		t.Fatalf("MakeMigrations(rename) error = %v", err)
	}

	if err := Migrate(context.Background(), applyOpts); err != nil {
		t.Fatalf("Migrate(rename) error = %v", err)
	}

	db, err = database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	var title string
	if err := db.Raw("SELECT title FROM guilds").Scan(&title).Error; err != nil {
		t.Fatalf("select title after rename: %v", err)
	}
	if title != "Alpha" {
		t.Fatalf("title = %q after rename, want the row preserved as \"Alpha\" (data loss = #299)", title)
	}
}
