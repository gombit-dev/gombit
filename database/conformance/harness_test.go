//go:build conformance

package conformance_test

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/migrations"
)

var (
	conformanceDriver = flag.String("conformance.driver", "sqlite", "database driver: sqlite, postgres, or mysql")
	conformanceDSN    = flag.String("conformance.dsn", "", "database DSN (sqlite defaults to a temp file when empty)")
)

type harness struct {
	t          *testing.T
	cfg        config.DatabaseConfig
	atlasBin   string
	root       string
	workDir    string
	migrateDir string
	opts       migrations.ApplyOptions
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	atlasBin := findAtlas(t)
	cfg := databaseConfig(t)
	root := projectRoot(t)
	workDir := t.TempDir()
	migrateDir := filepath.Join(workDir, "database", "migrations")
	if err := os.MkdirAll(migrateDir, 0o750); err != nil {
		t.Fatalf("mkdir migrations: %v", err)
	}

	cleanupSchema(t, cfg)
	t.Cleanup(func() { cleanupSchema(t, cfg) })

	h := &harness{
		t:          t,
		cfg:        cfg,
		atlasBin:   atlasBin,
		root:       root,
		workDir:    workDir,
		migrateDir: migrateDir,
		opts: migrations.ApplyOptions{
			WorkDir:      workDir,
			MigrationDir: "database/migrations",
			AtlasBinary:  atlasBin,
			Database:     cfg,
			Stdout:       new(bytes.Buffer),
			Stderr:       new(bytes.Buffer),
		},
	}
	h.prepareSchema()
	return h
}

func (h *harness) prepareSchema() {
	h.t.Helper()

	stderr := new(bytes.Buffer)
	err := migrations.MakeMigrations(context.Background(), migrations.Options{
		WorkDir:      h.root,
		Name:         "create_items",
		Driver:       h.cfg.Driver,
		MigrationDir: h.migrateDir,
		AtlasBinary:  h.atlasBin,
		Models:       conformanceModels(),
		Stdout:       io.Discard,
		Stderr:       stderr,
	})
	if err != nil {
		h.t.Fatalf("MakeMigrations() error = %v; stderr=%s", err, stderr.String())
	}

	ups, err := filepath.Glob(filepath.Join(h.migrateDir, "*.sql"))
	if err != nil {
		h.t.Fatalf("glob migrations: %v", err)
	}
	if len(ups) != 1 {
		h.t.Fatalf("migration files = %v, want exactly one Atlas-generated SQL file", ups)
	}

	version, name, err := migrations.ParseMigrationFilename(filepath.Base(ups[0]))
	if err != nil {
		h.t.Fatalf("ParseMigrationFilename: %v", err)
	}
	downDir := filepath.Join(h.migrateDir, "downs")
	if err := os.MkdirAll(downDir, 0o750); err != nil {
		h.t.Fatalf("mkdir downs: %v", err)
	}
	downPath := migrations.DownPath(h.migrateDir, version, name)
	down := "DROP TABLE IF EXISTS restricted_children;\nDROP TABLE IF EXISTS cascaded_children;\nDROP TABLE IF EXISTS nulled_children;\nDROP TABLE IF EXISTS soft_owner_children;\nDROP TABLE IF EXISTS owners;\nDROP TABLE IF EXISTS soft_owners;\nDROP TABLE IF EXISTS items;\n"
	if err := os.WriteFile(downPath, []byte(down), 0o600); err != nil {
		h.t.Fatalf("write down migration: %v", err)
	}

	stdout := new(bytes.Buffer)
	stderr = new(bytes.Buffer)
	h.opts.Stdout = stdout
	h.opts.Stderr = stderr
	if err := migrations.Migrate(context.Background(), h.opts); err != nil {
		h.t.Fatalf("Migrate() error = %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Applied") {
		h.t.Fatalf("migrate stdout = %q, want Applied", stdout.String())
	}
}

// conformanceModels are the fixtures the suite's one migration creates: Item,
// and the relation-deletion fixtures (#312).
func conformanceModels() []migrations.Model {
	var out []migrations.Model
	for _, name := range []string{"Item", "Owner", "RestrictedChild", "CascadedChild", "NulledChild", "SoftOwner", "SoftOwnerChild"} {
		out = append(out, migrations.Model{ImportPath: "github.com/gombit-dev/gombit/database/conformance/models", TypeName: name})
	}
	return out
}

func (h *harness) openDB() *database.DB {
	h.t.Helper()
	db, err := database.Open(h.cfg)
	if err != nil {
		h.t.Fatalf("database.Open() error = %v", err)
	}
	h.t.Cleanup(func() { _ = db.Close() })
	return db
}

func (h *harness) rollback() {
	h.t.Helper()
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	h.opts.Stdout = stdout
	h.opts.Stderr = stderr
	if err := migrations.Rollback(context.Background(), h.opts); err != nil {
		h.t.Fatalf("Rollback() error = %v; stderr=%s", err, stderr.String())
	}
}

func findAtlas(t *testing.T) string {
	t.Helper()
	if bin := strings.TrimSpace(os.Getenv("ATLAS_BINARY")); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("atlas")
	if err != nil {
		t.Skip("Atlas CLI not found; set ATLAS_BINARY to run conformance tests")
	}
	return bin
}

func databaseConfig(t *testing.T) config.DatabaseConfig {
	t.Helper()

	driver := config.DatabaseDriver(strings.TrimSpace(*conformanceDriver))
	dsn := strings.TrimSpace(*conformanceDSN)

	switch driver {
	case config.DatabaseDriverSQLite:
		if dsn == "" {
			dsn = "file:" + filepath.Join(t.TempDir(), "conformance.db") + "?cache=shared&_fk=1"
		}
	case config.DatabaseDriverPostgres, config.DatabaseDriverMySQL:
		if dsn == "" {
			t.Fatalf("-conformance.dsn is required for driver %q", driver)
		}
	default:
		t.Fatalf("unsupported -conformance.driver %q", driver)
	}

	cfg := config.DatabaseConfig{Driver: driver, DSN: dsn}
	if err := config.ValidateDatabase(cfg); err != nil {
		t.Fatalf("ValidateDatabase: %v", err)
	}
	return cfg
}

func projectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found from test working directory")
		}
		dir = parent
	}
}

func cleanupSchema(t *testing.T, cfg config.DatabaseConfig) {
	t.Helper()
	if err := migrations.DropSchema(context.Background(), migrations.ApplyOptions{
		Database: cfg,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}); err != nil {
		t.Fatalf("DropSchema cleanup: %v", err)
	}
}
