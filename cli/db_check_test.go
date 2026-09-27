package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/client"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/generate"
	"github.com/gombit-dev/gombit/migrations"
)

func TestCheckHelpListsTheLayers(t *testing.T) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"db", "check", "--help"}); err != nil {
		t.Fatalf("gombit db check --help: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"generated contract", "model registry", "migration directory", "migration safety",
		"models ↔ migrations", "pending migrations", "database schema", "TypeScript client",
		"--no-db", "--db-timeout", "--openapi-url", "--json", "non-zero",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("db check help missing %q:\n%s", want, out)
		}
	}
}

func TestWriteCheckReport(t *testing.T) {
	report := checkReport{Driver: config.DatabaseDriverSQLite, Layers: []checkLayer{
		{Name: "generated contract", Status: layerOK, Detail: "the *.gen.go match the models"},
		{Name: "pending migrations", Status: layerDrift, Detail: "migrations the database has not applied", Items: []string{"20260101000000_create_widgets"}, Fix: "gombit db migrate"},
		{Name: "TypeScript client", Status: layerSkipped, Detail: "pass --openapi-url", Fix: "never shown for a skip"},
	}}
	var out bytes.Buffer
	writeCheckReport(&out, report)
	for _, want := range []string{"ok       generated contract", "DRIFT    pending migrations", "- 20260101000000_create_widgets", "fix: gombit db migrate", "skipped  TypeScript client", "The schema chain is inconsistent."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "never shown") {
		t.Errorf("a skipped layer printed its fix:\n%s", out.String())
	}
	if !report.failed() {
		t.Fatal("a drifted layer does not fail the report")
	}

	report.Layers[1].Status = layerError
	if !report.failed() {
		t.Fatal("an errored layer does not fail the report")
	}
	report.Layers[1].Status = layerOK
	if report.failed() {
		t.Fatal("ok and skipped layers fail the report")
	}
	out.Reset()
	writeCheckReport(&out, report)
	if !strings.HasSuffix(out.String(), "\nThe schema chain is consistent.\n") {
		t.Fatalf("clean report does not end with the success line:\n%s", out.String())
	}
}

func TestCheckDatabaseRefusesAnotherDriver(t *testing.T) {
	pending, schemaLayer := checkDatabaseLayers(context.Background(), checkOptions{
		dir:      migrations.DirOptions{Driver: config.DatabaseDriverPostgres},
		database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "app.db"},
	}, true)
	if pending.Status != layerError || !strings.Contains(pending.Detail, "--driver postgres") {
		t.Fatalf("pending = %+v, want an error naming the driver mismatch", pending)
	}
	if schemaLayer.Status != layerSkipped {
		t.Fatalf("database schema = %+v, want skipped", schemaLayer)
	}
}

func TestCheckUnreachableDatabaseIsAnError(t *testing.T) {
	pending, schemaLayer := checkDatabaseLayers(context.Background(), checkOptions{
		dir:      migrations.DirOptions{WorkDir: t.TempDir(), Driver: config.DatabaseDriverPostgres, AtlasBinary: "atlas"},
		database: config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, DSN: "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"},
	}, true)
	if pending.Status != layerError || !strings.Contains(pending.Fix, "--no-db") {
		t.Fatalf("pending = %+v, want an error that points at --no-db", pending)
	}
	if schemaLayer.Status != layerSkipped {
		t.Fatalf("database schema = %+v, want skipped", schemaLayer)
	}
}

// TestCheckAtlasCLISQLiteWhenAvailable walks one database through the chain:
// pending, applied and clean, then changed by hand outside a migration.
func TestCheckAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		if atlasBin, err = exec.LookPath("atlas"); err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real db check test")
		}
	}
	app := t.TempDir()
	t.Chdir(app)
	if err := os.WriteFile("go.mod", []byte("module example.com/app\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The smallest tree the generator and the AutoMigrate reader accept.
	for path, src := range map[string]string{
		filepath.Join("cmd", "server", "main.go"):            "package main\n\nfunc main() {}\n",
		filepath.Join("internal", "platform", "database.go"): "package platform\n\nimport \"gorm.io/gorm\"\n\nfunc Migrate(db *gorm.DB) error { return db.AutoMigrate() }\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join("database", "migrations")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260101000000_create_widgets.sql"),
		[]byte("CREATE TABLE `widgets` (`id` integer NOT NULL, `name` text NOT NULL, PRIMARY KEY (`id`));\nCREATE INDEX `idx_widgets_name` ON `widgets` (`name`);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Database = config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(app, "app.db") + "?_fk=1"}
	prev := LoadConfig
	t.Cleanup(func() { LoadConfig = prev })
	LoadConfig = func() (config.Config, error) { return cfg, nil }
	run := func(args ...string) (string, error) {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), append(args, "--atlas-bin", atlasBin))
		return stdout.String() + stderr.String(), err
	}
	if out, err := run("db", "hash"); err != nil {
		t.Fatalf("db hash: %v\n%s", err, out)
	}

	out, err := run("db", "check")
	if err == nil || !strings.Contains(out, "DRIFT    pending migrations") || !strings.Contains(out, "20260101000000_create_widgets") || !strings.Contains(out, "fix: gombit db migrate") {
		t.Fatalf("db check before migrate: err = %v, out:\n%s", err, out)
	}
	if out, err := run("db", "migrate"); err != nil {
		t.Fatalf("db migrate: %v\n%s", err, out)
	}
	out, err = run("db", "check")
	if err != nil || !strings.Contains(out, "ok       database schema") || !strings.HasSuffix(out, "The schema chain is consistent.\n") {
		t.Fatalf("db check after migrate: err = %v, out:\n%s", err, out)
	}
	// A clean project prints the same report every run.
	if again, err := run("db", "check"); err != nil || again != out {
		t.Fatalf("db check is not deterministic: err = %v\nfirst:\n%s\nsecond:\n%s", err, out, again)
	}

	// Change the database by hand: a column added, an index dropped.
	db, err := database.Open(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"ALTER TABLE widgets ADD COLUMN extra text", "DROP INDEX idx_widgets_name"} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = db.Close()
	out, err = run("db", "check")
	if err == nil || !strings.Contains(out, "DRIFT    database schema") ||
		!strings.Contains(out, "add_column:widgets.extra") || !strings.Contains(out, "drop_index:widgets.idx_widgets_name") {
		t.Fatalf("db check after a hand edit: err = %v, out:\n%s", err, out)
	}
	if out, err := run("db", "check", "--no-db"); err != nil || !strings.Contains(out, "skipped  database schema      --no-db") {
		t.Fatalf("db check --no-db: err = %v, out:\n%s", err, out)
	}

	out, err = run("db", "check", "--json")
	if err == nil {
		t.Fatal("db check --json passed a drifted database")
	}
	var report checkReport
	if jerr := json.Unmarshal([]byte(out[:strings.LastIndex(out, "}")+1]), &report); jerr != nil {
		t.Fatalf("db check --json: %v\n%s", jerr, out)
	}
	statuses := map[string]string{}
	for _, l := range report.Layers {
		statuses[l.Name] = l.Status
	}
	if statuses["database schema"] != layerDrift || statuses["pending migrations"] != layerOK || statuses["TypeScript client"] != layerSkipped || len(report.Layers) != 8 {
		t.Fatalf("db check --json layers = %v", statuses)
	}
}

func TestCheckModelRegistryBothDirections(t *testing.T) {
	work := t.TempDir()
	platform := filepath.Join(work, "internal", "platform", "database.go")
	if err := os.MkdirAll(filepath.Dir(platform), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte(`package platform

import (
	"github.com/gombit-dev/gombit/database"

	"github.com/example/demo/internal/book"
	"github.com/example/demo/internal/product"
)

func AutoMigrate(db *database.DB) error {
	return db.AutoMigrate(&product.Product{}, &book.Book{})
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := migrations.DirOptions{WorkDir: work}
	product := migrations.Model{ImportPath: "github.com/example/demo/internal/product", TypeName: "Product"}
	book := migrations.Model{ImportPath: "github.com/example/demo/internal/book", TypeName: "Book"}
	note := migrations.Model{ImportPath: "github.com/example/demo/internal/note", TypeName: "Note"}
	registry := filepath.Join(work, "database", "migrations")
	if err := os.MkdirAll(registry, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := migrations.SaveRegistry(registry, []migrations.Model{product, note}); err != nil {
		t.Fatal(err)
	}
	l := checkModelRegistry(checkOptions{dir: dir})
	want := []string{
		"github.com/example/demo/internal/book.Book is auto-migrated but no migration registers it",
		"github.com/example/demo/internal/note.Note is in models.json but AutoMigrate does not list it",
	}
	if l.Status != layerDrift || strings.Join(l.Items, "\n") != strings.Join(want, "\n") || !strings.Contains(l.Fix, "--forget-model") {
		t.Fatalf("registry drift = %+v, want %v", l, want)
	}

	if err := migrations.SaveRegistry(registry, []migrations.Model{book, product}); err != nil {
		t.Fatal(err)
	}
	if l := checkModelRegistry(checkOptions{dir: dir}); l.Status != layerOK {
		t.Fatalf("matching registry = %+v, want ok", l)
	}

	if l := checkModelRegistry(checkOptions{dir: migrations.DirOptions{WorkDir: t.TempDir()}}); l.Status != layerSkipped {
		t.Fatalf("no platform file = %+v, want skipped", l)
	}
}

func stubInspectDatabase(t *testing.T, fn func(context.Context, migrations.ApplyOptions) (migrations.DatabaseState, error)) {
	t.Helper()
	prev := inspectDatabase
	t.Cleanup(func() { inspectDatabase = prev })
	inspectDatabase = fn
}

func TestCheckDatabaseHistoryGapsSkipTheSchemaComparison(t *testing.T) {
	sqlite := checkOptions{
		dir:      migrations.DirOptions{WorkDir: t.TempDir(), Driver: config.DatabaseDriverSQLite},
		database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "app.db"},
	}
	for _, tc := range []struct {
		name        string
		state       migrations.DatabaseState
		wantItems   []string
		wantFix     string
		wantSkipped string
	}{
		{
			name: "an older migration is pending",
			state: migrations.DatabaseState{
				LastApplied: "20260103000000",
				Pending:     []migrations.MigrationFile{{Version: "20260102000000", Name: "add_note"}},
			},
			wantItems:   []string{"20260102000000_add_note"},
			wantFix:     "gombit db migrate",
			wantSkipped: "20260102000000 is pending but older than the last applied migration 20260103000000",
		},
		{
			name:        "an applied migration has no file",
			state:       migrations.DatabaseState{LastApplied: "20260101000000", Unknown: []string{"20260102000000"}},
			wantItems:   []string{"20260102000000 is applied but has no file in the migration directory"},
			wantFix:     "restore the missing migration files",
			wantSkipped: "the database applied migrations the directory lacks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInspectDatabase(t, func(context.Context, migrations.ApplyOptions) (migrations.DatabaseState, error) {
				return tc.state, nil
			})
			pending, schemaLayer := checkDatabaseLayers(context.Background(), sqlite, true)
			if pending.Status != layerDrift || strings.Join(pending.Items, "\n") != strings.Join(tc.wantItems, "\n") || !strings.Contains(pending.Fix, tc.wantFix) {
				t.Fatalf("pending = %+v", pending)
			}
			if schemaLayer.Status != layerSkipped || schemaLayer.Detail != tc.wantSkipped {
				t.Fatalf("database schema = %+v, want skipped: %s", schemaLayer, tc.wantSkipped)
			}
		})
	}
}

func TestCheckDatabaseTimesOut(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubInspectDatabase(t, func(context.Context, migrations.ApplyOptions) (migrations.DatabaseState, error) {
		<-release // a connect that ignores the context
		return migrations.DatabaseState{}, nil
	})
	pending, schemaLayer := checkDatabaseLayers(context.Background(), checkOptions{
		dir:       migrations.DirOptions{Driver: config.DatabaseDriverSQLite},
		database:  config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "app.db"},
		dbTimeout: 20 * time.Millisecond,
	}, true)
	if pending.Status != layerError || !strings.Contains(pending.Detail, "--db-timeout") || schemaLayer.Status != layerSkipped {
		t.Fatalf("pending = %+v, schema = %+v, want a timeout error", pending, schemaLayer)
	}
}

func TestCheckTellsDriftFromFailure(t *testing.T) {
	prevGen, prevClient := generateContract, checkClientDrift
	t.Cleanup(func() { generateContract, checkClientDrift = prevGen, prevClient })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openapi":"3.1.0"}`))
	}))
	t.Cleanup(srv.Close)
	o := checkOptions{dir: migrations.DirOptions{WorkDir: t.TempDir()}, openapiURL: srv.URL}

	for _, tc := range []struct {
		err        error
		wantStatus string
		wantFix    string
	}{
		{fmt.Errorf("%w; run `gombit generate`: internal/book/dto.gen.go", generate.ErrStale), layerDrift, "gombit generate"},
		{errors.New("resourcegen: parse internal/book/book.go: expected declaration"), layerError, ""},
	} {
		generateContract = func(context.Context, generate.Options) error { return tc.err }
		if l := checkGeneratedContract(context.Background(), o); l.Status != tc.wantStatus || l.Fix != tc.wantFix {
			t.Errorf("generated contract for %q = %+v, want %s", tc.err, l, tc.wantStatus)
		}
	}

	for _, tc := range []struct {
		err        error
		wantStatus string
		wantFix    string
	}{
		{fmt.Errorf("%w in frontend/src/api/generated/schema.ts", client.ErrDrift), layerDrift, "gombit client check --write --url " + srv.URL},
		{fmt.Errorf("client: read committed spec: %w", os.ErrNotExist), layerError, "commit a first client: gombit client check --write --url " + srv.URL},
		{errors.New("client: npx: executable file not found"), layerError, ""},
	} {
		var gotOpts client.DriftOptions
		checkClientDrift = func(_ context.Context, opts client.DriftOptions) error { gotOpts = opts; return tc.err }
		l := checkClient(context.Background(), o)
		if l.Status != tc.wantStatus || l.Fix != tc.wantFix {
			t.Errorf("client for %q = %+v, want %s", tc.err, l, tc.wantStatus)
		}
		// The app's own client, not the framework's sample fixtures.
		if gotOpts.SpecPath != client.DefaultSpecPath || gotOpts.OutDir != client.DefaultOutDir || len(gotOpts.SpecBytes) == 0 {
			t.Fatalf("CheckDrift options = %+v, want the app's spec and client paths", gotOpts)
		}
	}
}
