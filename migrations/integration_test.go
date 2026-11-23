//go:build integration

package migrations

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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

func TestMigrateLeavesPartialAtlasRevisionPendingMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -migrations.mysql-dsn to run MySQL partial-revision integration test")
	}
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the MySQL partial-revision integration test")
		}
	}

	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	firstVersion := time.Now().UTC().Format("20060102150405")
	partialVersion := time.Now().UTC().Add(time.Second).Format("20060102150405")
	prefix := "gombit_partial_" + partialVersion
	firstTable := prefix + "_first"
	partialTable := prefix + "_partial"
	afterTable := prefix + "_after"
	missingTable := prefix + "_missing"
	firstPath := filepath.Join(migrationDir, firstVersion+"_create_first.sql")
	partialPath := filepath.Join(migrationDir, partialVersion+"_create_partial.sql")
	writeFile(t, firstPath, "CREATE TABLE "+firstTable+" (id BIGINT PRIMARY KEY);\n")
	writeFile(t, partialPath,
		"CREATE TABLE IF NOT EXISTS "+partialTable+" (id BIGINT PRIMARY KEY);\n"+
			"INSERT INTO "+missingTable+" (id) VALUES (1);\n"+
			"CREATE TABLE IF NOT EXISTS "+afterTable+" (id BIGINT PRIMARY KEY);\n")
	if err := (execRunner{}).Run(context.Background(), workDir, atlasBin, []string{
		"migrate", "hash", "--dir", "file://" + filepath.ToSlash(migrationDir),
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("atlas migrate hash: %v", err)
	}

	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverMySQL, DSN: *mysqlDSN}
	cleanupDriverDB(t, cfg)
	t.Cleanup(func() { cleanupDriverDB(t, cfg) })

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	runner := &countingApplyRunner{inner: execRunner{}}
	opts := ApplyOptions{
		WorkDir: workDir, MigrationDir: "database/migrations", AtlasBinary: atlasBin,
		Database: cfg, Stdout: stdout, Stderr: stderr, runner: runner,
	}
	if err := Migrate(context.Background(), opts); err == nil {
		t.Fatal("first Migrate() error = nil, want Atlas's partial-apply failure")
	}
	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	var applied, total int
	var atlasError string
	if err := db.Table(atlasRevisionsTable).
		Select("applied, total, error").Where("version = ?", partialVersion).
		Row().Scan(&applied, &total, &atlasError); err != nil {
		_ = db.Close()
		t.Fatalf("read partial Atlas revision: %v", err)
	}
	if applied >= total || atlasError == "" {
		_ = db.Close()
		t.Fatalf("partial Atlas revision = applied %d / %d, error %q; want incomplete revision with error", applied, total, atlasError)
	}
	revisions, err := listRevisions(db.DB)
	if err != nil {
		_ = db.Close()
		t.Fatalf("listRevisions() error = %v", err)
	}
	if len(revisions) != 1 || revisions[0].Version != firstVersion {
		_ = db.Close()
		t.Fatalf("framework revisions = %#v, want only completed migration %s", revisions, firstVersion)
	}
	firstBatch := revisions[0].Batch
	_ = db.Close()

	status := new(bytes.Buffer)
	opts.Stdout = status
	opts.Stderr = io.Discard
	if err := Status(context.Background(), opts); err != nil {
		t.Fatalf("Status() after partial apply: %v", err)
	}
	if !strings.Contains(status.String(), "pending") || !strings.Contains(status.String(), partialVersion) {
		t.Fatalf("Status() = %q, want partial version listed as pending", status.String())
	}
	if err := Migrate(context.Background(), opts); err == nil {
		t.Fatal("second Migrate() error = nil, want the partial migration to be retried")
	}
	if runner.applyCalls != 2 {
		t.Fatalf("Atlas apply calls after retry = %d, want 2", runner.applyCalls)
	}

	writeFile(t, partialPath,
		"CREATE TABLE IF NOT EXISTS "+partialTable+" (id BIGINT PRIMARY KEY);\n"+
			"INSERT INTO "+partialTable+" (id) VALUES (1);\n"+
			"CREATE TABLE IF NOT EXISTS "+afterTable+" (id BIGINT PRIMARY KEY);\n")
	if err := (execRunner{}).Run(context.Background(), workDir, atlasBin, []string{
		"migrate", "hash", "--dir", "file://" + filepath.ToSlash(migrationDir),
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("atlas migrate hash after fixing migration: %v", err)
	}
	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() after fixing partial migration: %v; stderr=%q", err, stderr.String())
	}
	if runner.applyCalls != 3 {
		t.Fatalf("Atlas apply calls after repair = %d, want 3", runner.applyCalls)
	}
	repairedDB, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() after repair: %v", err)
	}
	defer func() { _ = repairedDB.Close() }()
	revisions, err = listRevisions(repairedDB.DB)
	if err != nil {
		t.Fatalf("listRevisions() after repair: %v", err)
	}
	if len(revisions) != 2 || revisions[0].Version != firstVersion || revisions[1].Version != partialVersion {
		t.Fatalf("revisions after repair = %#v, want both completed migrations", revisions)
	}
	if revisions[0].Batch != firstBatch || revisions[1].Batch != firstBatch+1 {
		t.Fatalf("batches after repair = %d and %d, want %d and %d", revisions[0].Batch, revisions[1].Batch, firstBatch, firstBatch+1)
	}
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
	t   *testing.T
	cfg config.DatabaseConfig
}

func (r *sqlApplyRunner) Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	r.t.Helper()
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

	if err := db.Exec(`
CREATE TABLE IF NOT EXISTS atlas_schema_revisions (
  version varchar(255) PRIMARY KEY,
  description text,
  type varchar(255),
  applied_at timestamp NULL
)`).Error; err != nil {
		return err
	}

	files, err := ListMigrationFiles(migrationDir)
	if err != nil {
		return err
	}
	var existing []string
	if err := db.Table(atlasRevisionsTable).Select("version").Find(&existing).Error; err != nil {
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
			"INSERT INTO atlas_schema_revisions (version, description, type, applied_at) VALUES (?, ?, ?, ?)",
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
