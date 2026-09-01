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

func TestMigrateRollbackStatusSQLiteRoundTrip(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL);")
	writeFile(t, filepath.Join(migrationDir, "downs", "20260101000000_create_widgets.down.sql"),
		"DROP TABLE widgets;")
	writeFile(t, filepath.Join(migrationDir, "20260102000000_add_widget_note.sql"),
		"ALTER TABLE widgets ADD COLUMN note TEXT;")
	writeFile(t, filepath.Join(migrationDir, "downs", "20260102000000_add_widget_note.down.sql"),
		"ALTER TABLE widgets DROP COLUMN note;")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	runner := &applyFakeAtlas{t: t}
	fixedNow := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	stdout := new(bytes.Buffer)

	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: "database/migrations",
		AtlasBinary:  "atlas",
		Database:     cfg,
		Stdout:       stdout,
		Stderr:       io.Discard,
		runner:       runner,
		now:          func() time.Time { return fixedNow },
	}

	if err := Status(context.Background(), opts); err != nil {
		t.Fatalf("Status() before migrate error = %v", err)
	}
	if !strings.Contains(stdout.String(), "pending") || !strings.Contains(stdout.String(), "20260101000000") {
		t.Fatalf("status before migrate = %q, want pending migrations", stdout.String())
	}
	stdout.Reset()

	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Applied 2 migration(s) in batch 1") {
		t.Fatalf("migrate stdout = %q", stdout.String())
	}
	if len(runner.applyCalls) != 1 {
		t.Fatalf("atlas apply calls = %d, want 1", len(runner.applyCalls))
	}
	stdout.Reset()

	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets table after migrate")
	}
	revisions, err := listRevisions(db.DB)
	if err != nil {
		t.Fatalf("listRevisions() error = %v", err)
	}
	if len(revisions) != 2 || revisions[0].Batch != 1 || revisions[0].AppliedAt.UTC() != fixedNow {
		t.Fatalf("revisions = %#v", revisions)
	}
	if !db.Migrator().HasTable(atlasRevisionsTable) {
		t.Fatal("expected atlas_schema_revisions after fake apply")
	}

	if err := Status(context.Background(), opts); err != nil {
		t.Fatalf("Status() after migrate error = %v", err)
	}
	if !strings.Contains(stdout.String(), "applied") || strings.Contains(stdout.String(), "pending\t") {
		t.Fatalf("status after migrate = %q", stdout.String())
	}
	stdout.Reset()

	if err := Rollback(context.Background(), opts); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Rolled back batch 1 (2 migration(s))") {
		t.Fatalf("rollback stdout = %q", stdout.String())
	}
	if db.Migrator().HasTable("widgets") {
		t.Fatal("widgets table should be gone after rollback")
	}
	revisions, err = listRevisions(db.DB)
	if err != nil {
		t.Fatalf("listRevisions() after rollback error = %v", err)
	}
	if len(revisions) != 0 {
		t.Fatalf("revisions after rollback = %#v", revisions)
	}
	var atlasCount int64
	if err := db.Table(atlasRevisionsTable).Count(&atlasCount).Error; err != nil {
		t.Fatalf("count atlas revisions: %v", err)
	}
	if atlasCount != 0 {
		t.Fatalf("atlas revisions after rollback = %d, want 0", atlasCount)
	}
	stdout.Reset()

	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() after rollback error = %v", err)
	}
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets table after re-migrate")
	}
}

func TestMigrateNoPending(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	stdout := new(bytes.Buffer)
	runner := &applyFakeAtlas{t: t}

	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: migrationDir,
		Database:     cfg,
		Stdout:       stdout,
		runner:       runner,
	}
	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "No pending migrations") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(runner.applyCalls) != 0 {
		t.Fatalf("unexpected atlas apply calls: %d", len(runner.applyCalls))
	}
}

func TestRollbackMissingDownFails(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY);")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: migrationDir,
		Database:     cfg,
		Stdout:       io.Discard,
		runner:       &applyFakeAtlas{t: t},
	}
	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	err := Rollback(context.Background(), opts)
	if err == nil {
		t.Fatal("Rollback() error = nil, want missing down error")
	}
	if !strings.Contains(err.Error(), "missing down migration") {
		t.Fatalf("Rollback() error = %q", err)
	}
}

func TestRollbackNothingToDo(t *testing.T) {
	workDir := t.TempDir()
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	stdout := new(bytes.Buffer)
	err := Rollback(context.Background(), ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: filepath.Join(workDir, "database", "migrations"),
		Database:     config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn},
		Stdout:       stdout,
	})
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "Nothing to roll back") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestMigrateRecordsOnlyVersionsPresentInAtlas(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY);")
	writeFile(t, filepath.Join(migrationDir, "20260102000000_create_gadgets.sql"),
		"CREATE TABLE gadgets (id INTEGER PRIMARY KEY);")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	stderr := new(bytes.Buffer)
	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: migrationDir,
		Database:     cfg,
		Stdout:       io.Discard,
		Stderr:       stderr,
		runner: &applyFakeAtlas{
			t:            t,
			applyVersions: map[string]bool{"20260101000000": true},
		},
	}
	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	revisions, err := listRevisions(db.DB)
	if err != nil {
		t.Fatalf("listRevisions() error = %v", err)
	}
	if len(revisions) != 1 || revisions[0].Version != "20260101000000" {
		t.Fatalf("revisions = %#v, want only 20260101000000", revisions)
	}
	if !strings.Contains(stderr.String(), "recording 1 of 2 pending") {
		t.Fatalf("stderr = %q, want partial-record warning", stderr.String())
	}
}

// Note: a happy-path multi-statement SQLite test is intentionally not here.
// mattn/go-sqlite3 already executes a multi-statement string sequentially in
// one Exec, and Rollback runs inside a transaction on SQLite, so such a test
// would pass identically before and after the statement-splitting fix for
// #249 and prove nothing. The real regression coverage for that fix is
// TestMigrateRollbackStatusMySQL (migrations/integration_test.go, requires a
// live MySQL DSN) and the statement-numbering check below.

func TestRollbackMidStatementFailureNamesStatementNumber(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY);")
	writeFile(t, filepath.Join(migrationDir, "downs", "20260101000000_create_widgets.down.sql"),
		"DROP TABLE widgets;\nTHIS IS NOT VALID SQL;")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: migrationDir,
		Database:     cfg,
		Stdout:       io.Discard,
		runner:       &applyFakeAtlas{t: t},
	}
	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	err := Rollback(context.Background(), opts)
	if err == nil {
		t.Fatal("Rollback() error = nil, want second-statement failure")
	}
	if !strings.Contains(err.Error(), "execute statement 2 of") {
		t.Fatalf("Rollback() error = %q, want it to name the failing statement", err)
	}
	if !strings.Contains(err.Error(), "framework_migrations unchanged") {
		t.Fatalf("Rollback() error = %q, want unchanged revisions guidance", err)
	}

	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if !db.Migrator().HasTable("widgets") {
		t.Fatal("expected widgets table to remain: rollback runs in a transaction on SQLite, so the successful first statement must be rolled back too")
	}
}

func TestRollbackMidBatchFailureAbortsTransaction(t *testing.T) {
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(migrationDir, "20260101000000_create_widgets.sql"),
		"CREATE TABLE widgets (id INTEGER PRIMARY KEY);")
	writeFile(t, filepath.Join(migrationDir, "downs", "20260101000000_create_widgets.down.sql"),
		"THIS IS NOT VALID SQL;")
	writeFile(t, filepath.Join(migrationDir, "20260102000000_create_gadgets.sql"),
		"CREATE TABLE gadgets (id INTEGER PRIMARY KEY);")
	writeFile(t, filepath.Join(migrationDir, "downs", "20260102000000_create_gadgets.down.sql"),
		"DROP TABLE gadgets;")

	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	cfg := config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}
	opts := ApplyOptions{
		WorkDir:      workDir,
		MigrationDir: migrationDir,
		Database:     cfg,
		Stdout:       io.Discard,
		runner:       &applyFakeAtlas{t: t},
	}
	if err := Migrate(context.Background(), opts); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	err := Rollback(context.Background(), opts)
	if err == nil {
		t.Fatal("Rollback() error = nil, want mid-batch failure")
	}
	if !strings.Contains(err.Error(), "framework_migrations unchanged") {
		t.Fatalf("Rollback() error = %q, want unchanged revisions guidance", err)
	}

	db, err := database.Open(cfg)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if !db.Migrator().HasTable("widgets") || !db.Migrator().HasTable("gadgets") {
		t.Fatal("expected both tables to remain after aborted transactional rollback")
	}
	revisions, err := listRevisions(db.DB)
	if err != nil {
		t.Fatalf("listRevisions() error = %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("revisions len = %d, want 2 after aborted rollback", len(revisions))
	}
}

type applyFakeAtlas struct {
	t             *testing.T
	applyCalls    [][]string
	applyVersions map[string]bool
}

func (r *applyFakeAtlas) Run(ctx context.Context, dir string, name string, args []string, stdout io.Writer, stderr io.Writer) error {
	r.t.Helper()
	if len(args) < 2 || args[0] != "migrate" {
		return fmt.Errorf("unexpected args: %v", args)
	}
	switch args[1] {
	case "apply":
		r.applyCalls = append(r.applyCalls, append([]string{}, args...))
		return r.apply(dir, args, stdout)
	case "status":
		_, _ = io.WriteString(stdout, "fake atlas status\n")
		return nil
	default:
		return fmt.Errorf("unexpected atlas subcommand: %v", args)
	}
}

func (r *applyFakeAtlas) apply(workDir string, args []string, stdout io.Writer) error {
	r.t.Helper()
	urlValue, dirURL, err := parseAtlasURLAndDir(args)
	if err != nil {
		return err
	}
	migrationDir := strings.TrimPrefix(dirURL, "file://")
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(workDir, migrationDir)
	}

	dsn := strings.TrimPrefix(urlValue, "sqlite://")
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + dsn,
	})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := db.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  version varchar(255) PRIMARY KEY,
  description text,
  type varchar(255),
  applied_at datetime
)`, atlasRevisionsTable)).Error; err != nil {
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
		if r.applyVersions != nil && !r.applyVersions[file.Version] {
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
			fmt.Sprintf("INSERT INTO %s (version, description, type, applied_at) VALUES (?, ?, ?, ?)", atlasRevisionsTable),
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

func parseAtlasURLAndDir(args []string) (atlasURL string, dirURL string, err error) {
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--url":
			atlasURL = args[i+1]
		case "--dir":
			dirURL = args[i+1]
		}
	}
	if atlasURL == "" || dirURL == "" {
		return "", "", fmt.Errorf("missing --url/--dir in %v", args)
	}
	return atlasURL, dirURL, nil
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
