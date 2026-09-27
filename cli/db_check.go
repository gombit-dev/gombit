package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gombit-dev/gombit/client"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/generate"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
	"github.com/gombit-dev/gombit/resourcegen"
)

// Layer statuses. Only ok and skipped pass.
const (
	layerOK      = "ok"
	layerDrift   = "drift"
	layerError   = "error"
	layerSkipped = "skipped"
)

// checkLayer is one link of the chain `gombit db check` validates.
type checkLayer struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Detail string   `json:"detail"`
	Items  []string `json:"items,omitempty"`
	Fix    string   `json:"fix,omitempty"`
}

type checkReport struct {
	Driver config.DatabaseDriver `json:"driver"`
	Layers []checkLayer          `json:"layers"`
}

func (r checkReport) failed() bool {
	for _, l := range r.Layers {
		if l.Status == layerDrift || l.Status == layerError {
			return true
		}
	}
	return false
}

func newCheckCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := silence(&cobra.Command{
		Use:   "check",
		Short: "Validate the whole schema chain: models, generated contract, migrations, database",
		Long: `Validate every link from the models to the database, in one non-interactive
run for local development and CI (SCHEMA-6):

  generated contract   the *.gen.go match the models ('gombit generate --check')
  model registry       the models AutoMigrate lists match models.json
  migration directory  atlas.sum matches and every migration applies to an
                       empty dev database
  migration safety     every destructive or unsafe change a migration makes is
                       acknowledged ('gombit db lint')
  models ↔ migrations  the models declare nothing the migrations lack
                       ('gombit db plan')
  pending migrations   the database has applied every migration
  database schema      the database has the schema its applied migrations build
                       (a column added by hand, an index dropped outside a
                       migration)
  TypeScript client    the committed client matches the running app's
                       /openapi.json (only with --openapi-url)

Each layer reports ok, drift, error, or skipped, with the command that fixes
it. The command exits non-zero when any layer drifts or errors. --no-db skips
the two database layers (a CI job without a database); a database that cannot
be read within --db-timeout is an error, not a skip. A later layer is skipped when an earlier one
makes it meaningless (the migrations are not classified while the directory is
inconsistent).`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("gombit db check: unexpected argument %q", args[0])
			}
			opts, err := dirFlags(cmd)
			if err != nil {
				return err
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			noDB, err := cmd.Flags().GetBool("no-db")
			if err != nil {
				return err
			}
			openapiURL, err := cmd.Flags().GetString("openapi-url")
			if err != nil {
				return err
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}
			dbTimeout, err := cmd.Flags().GetDuration("db-timeout")
			if err != nil {
				return err
			}
			report := runCheck(cmd.Context(), checkOptions{dir: opts, database: cfg.Database, noDB: noDB, dbTimeout: dbTimeout, openapiURL: openapiURL})
			if asJSON {
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return fmt.Errorf("gombit db check: marshal: %w", err)
				}
				if _, err := stdout.Write(append(data, '\n')); err != nil {
					return err
				}
			} else {
				writeCheckReport(stdout, report)
			}
			if report.failed() {
				return errors.New("gombit db check: the schema chain is inconsistent")
			}
			return nil
		},
	})
	bindDirFlags(cmd)
	cmd.Flags().Bool("no-db", false, "skip the pending-migrations and database-schema layers")
	cmd.Flags().Duration("db-timeout", 30*time.Second, "how long to wait for the application database (0 waits indefinitely)")
	cmd.Flags().String("openapi-url", "", "a running app's /openapi.json, to check the committed TypeScript client against it")
	cmd.Flags().Bool("json", false, "print the report as JSON")
	return cmd
}

type checkOptions struct {
	dir        migrations.DirOptions
	database   config.DatabaseConfig
	noDB       bool
	dbTimeout  time.Duration
	openapiURL string
}

// runCheck runs the layers in their fixed order.
func runCheck(ctx context.Context, o checkOptions) checkReport {
	r := checkReport{Driver: o.dir.Driver}
	r.Layers = append(r.Layers, checkGeneratedContract(ctx, o))
	r.Layers = append(r.Layers, checkModelRegistry(o))
	lint, dirLayer, safetyLayer := checkMigrationDirectory(ctx, o)
	r.Layers = append(r.Layers, dirLayer, safetyLayer)
	dirOK := lint.Integrity == ""
	r.Layers = append(r.Layers, checkModelsVsMigrations(ctx, o, dirOK))
	pending, schemaLayer := checkDatabaseLayers(ctx, o, dirOK)
	r.Layers = append(r.Layers, pending, schemaLayer)
	r.Layers = append(r.Layers, checkClient(ctx, o))
	return r
}

func checkGeneratedContract(ctx context.Context, o checkOptions) checkLayer {
	l := checkLayer{Name: "generated contract"}
	var out bytes.Buffer
	err := generateContract(ctx, generate.Options{WorkDir: o.dir.WorkDir, Check: true, Stdout: &out, Stderr: &out})
	switch {
	case errors.Is(err, generate.ErrStale):
		l.Status, l.Detail, l.Fix = layerDrift, oneLine(err.Error()), "gombit generate"
	case err != nil:
		l.Status, l.Detail = layerError, oneLine(err.Error())
	case strings.Contains(out.String(), "no model-first resources"):
		l.Status, l.Detail = layerOK, "no model-first resources"
	default:
		l.Status, l.Detail = layerOK, "the *.gen.go match the models"
	}
	return l
}

func checkModelRegistry(o checkOptions) checkLayer {
	l := checkLayer{Name: "model registry"}
	auto, ok, err := resourcegen.AutoMigrateModels(o.dir.WorkDir)
	if err != nil {
		l.Status, l.Detail = layerError, oneLine(err.Error())
		return l
	}
	if !ok {
		l.Status, l.Detail = layerSkipped, "no internal/platform/database.go AutoMigrate to compare with"
		return l
	}
	registered, err := migrations.LoadRegistry(registryDir(o.dir))
	if err != nil {
		l.Status, l.Detail = layerError, oneLine(err.Error())
		return l
	}
	inRegistry := map[migrations.Model]bool{}
	for _, m := range registered {
		inRegistry[m] = true
	}
	inAuto := map[migrations.Model]bool{}
	for _, m := range auto {
		inAuto[m] = true
		if !inRegistry[m] {
			l.Items = append(l.Items, fmt.Sprintf("%s.%s is auto-migrated but no migration registers it", m.ImportPath, m.TypeName))
		}
	}
	for _, m := range registered {
		if !inAuto[m] {
			l.Items = append(l.Items, fmt.Sprintf("%s.%s is in models.json but AutoMigrate does not list it", m.ImportPath, m.TypeName))
		}
	}
	if len(l.Items) > 0 {
		sort.Strings(l.Items)
		l.Status, l.Detail = layerDrift, "AutoMigrate and models.json disagree"
		l.Fix = "migrate an auto-migrated model with gombit db makemigrations <name> --model <import.Type>; " +
			"add a registered one to AutoMigrate, or retire it with --forget-model <import.Type>"
		return l
	}
	l.Status, l.Detail = layerOK, fmt.Sprintf("%d model(s) in AutoMigrate and models.json", len(auto))
	return l
}

func checkMigrationDirectory(ctx context.Context, o checkOptions) (schemaplan.LintReport, checkLayer, checkLayer) {
	dirLayer := checkLayer{Name: "migration directory"}
	safety := checkLayer{Name: "migration safety"}
	report, err := schemaplan.Lint(ctx, schemaplan.LintOptions{
		WorkDir: o.dir.WorkDir, Driver: o.dir.Driver, MigrationDir: o.dir.MigrationDir, AtlasBinary: o.dir.AtlasBinary, Stderr: io.Discard,
	})
	if err != nil {
		report.Integrity = err.Error()
		dirLayer.Status, dirLayer.Detail = layerError, oneLine(err.Error())
		safety.Status, safety.Detail = layerSkipped, "the migration directory could not be read"
		return report, dirLayer, safety
	}
	switch {
	case report.Integrity != "":
		dirLayer.Status, dirLayer.Detail, dirLayer.Fix = layerDrift, oneLine(report.Integrity), "gombit db repair"
	case len(report.Misplaced) > 0:
		dirLayer.Status, dirLayer.Detail, dirLayer.Items = layerDrift, "files that are not up migrations", report.Misplaced
		dirLayer.Fix = "move a down file to downs/<version>_<name>.down.sql, or rename it <version>_<name>.sql"
	default:
		dirLayer.Status, dirLayer.Detail = layerOK, fmt.Sprintf("atlas.sum matches; %d migration(s) apply to an empty database", len(report.Migrations))
	}
	if report.Integrity != "" {
		safety.Status, safety.Detail = layerSkipped, "the migration directory is inconsistent"
		return report, dirLayer, safety
	}
	if pending := report.Unacknowledged(); len(pending) > 0 {
		safety.Status, safety.Detail = layerDrift, "destructive or unsafe changes no -- gombit:allow line acknowledges"
		for _, p := range pending {
			safety.Items = append(safety.Items, p.File+": "+p.Step.ID)
		}
		safety.Fix = "handle the data, add -- gombit:allow <id> to the migration, then gombit db repair"
		return report, dirLayer, safety
	}
	safety.Status, safety.Detail = layerOK, "every destructive or unsafe change is acknowledged"
	return report, dirLayer, safety
}

func checkModelsVsMigrations(ctx context.Context, o checkOptions, dirOK bool) checkLayer {
	l := checkLayer{Name: "models ↔ migrations"}
	if !dirOK {
		l.Status, l.Detail = layerSkipped, "the migration directory is inconsistent"
		return l
	}
	registered, err := migrations.LoadRegistry(registryDir(o.dir))
	if err != nil {
		l.Status, l.Detail = layerError, oneLine(err.Error())
		return l
	}
	if len(registered) == 0 {
		l.Status, l.Detail = layerSkipped, "no models are registered in models.json"
		return l
	}
	in, err := migrations.Inspect(ctx, migrations.InspectOptions{
		WorkDir: o.dir.WorkDir, Driver: o.dir.Driver, MigrationDir: o.dir.MigrationDir, AtlasBinary: o.dir.AtlasBinary, Stderr: io.Discard,
	})
	if err != nil {
		l.Status, l.Detail = layerError, oneLine(err.Error())
		return l
	}
	plan, err := schemaplan.Build(in)
	if err != nil {
		l.Status, l.Detail = layerError, oneLine(err.Error())
		return l
	}
	if len(plan.Steps) > 0 {
		l.Status, l.Detail = layerDrift, "the models declare changes no migration captures"
		for _, s := range plan.Steps {
			l.Items = append(l.Items, s.ID)
		}
		l.Fix = "gombit db plan, then gombit db makemigrations <name>"
		return l
	}
	l.Status, l.Detail = layerOK, "the models match the migration directory"
	return l
}

func checkDatabaseLayers(ctx context.Context, o checkOptions, dirOK bool) (checkLayer, checkLayer) {
	pending := checkLayer{Name: "pending migrations"}
	schemaLayer := checkLayer{Name: "database schema"}
	if o.noDB {
		pending.Status, pending.Detail = layerSkipped, "--no-db"
		schemaLayer.Status, schemaLayer.Detail = layerSkipped, "--no-db"
		return pending, schemaLayer
	}
	if o.database.Driver != o.dir.Driver {
		pending.Status = layerError
		pending.Detail = fmt.Sprintf("--driver %s differs from the configured database driver %s", o.dir.Driver, o.database.Driver)
		pending.Fix = "drop --driver, or pass --no-db"
		schemaLayer.Status, schemaLayer.Detail = layerSkipped, "the database could not be read"
		return pending, schemaLayer
	}
	state, err := readDatabase(ctx, o)
	if err != nil {
		pending.Status, pending.Detail = layerError, oneLine(err.Error())
		pending.Fix = "configure GOMBIT_DATABASE_*, or pass --no-db where no database is available"
		schemaLayer.Status, schemaLayer.Detail = layerSkipped, "the database could not be read"
		return pending, schemaLayer
	}
	for _, v := range state.Unknown {
		pending.Items = append(pending.Items, v+" is applied but has no file in the migration directory")
	}
	for _, f := range state.Pending {
		pending.Items = append(pending.Items, f.Version+"_"+f.Name)
	}
	switch {
	case len(state.Unknown) > 0:
		pending.Status, pending.Detail = layerDrift, "the database's migration history and the directory disagree"
		pending.Fix = "restore the missing migration files (an applied migration must not be renamed or deleted)"
		if len(state.Pending) > 0 {
			pending.Fix += ", then gombit db migrate"
		}
	case len(state.Pending) > 0:
		pending.Status, pending.Detail, pending.Fix = layerDrift, "migrations the database has not applied", "gombit db migrate"
	default:
		pending.Status, pending.Detail = layerOK, "every migration is applied"
	}
	if !dirOK {
		schemaLayer.Status, schemaLayer.Detail = layerSkipped, "the migration directory is inconsistent"
		return pending, schemaLayer
	}
	// The expected schema is the directory replayed up to the last applied
	// version. It is not what the database should have when that history has
	// gaps: an older migration still pending, or an applied one gone.
	if len(state.Unknown) > 0 {
		schemaLayer.Status, schemaLayer.Detail = layerSkipped, "the database applied migrations the directory lacks"
		return pending, schemaLayer
	}
	for _, f := range state.Pending {
		if f.Version < state.LastApplied {
			schemaLayer.Status = layerSkipped
			schemaLayer.Detail = fmt.Sprintf("%s is pending but older than the last applied migration %s", f.Version, state.LastApplied)
			return pending, schemaLayer
		}
	}
	expected, err := migrations.InspectDirAt(ctx, o.dir, state.LastApplied)
	if err != nil {
		schemaLayer.Status, schemaLayer.Detail = layerError, oneLine(err.Error())
		return pending, schemaLayer
	}
	steps, err := schemaplan.DatabaseDrift(o.dir.Driver, expected, state.Schema, migrations.BookkeepingTables)
	if err != nil {
		schemaLayer.Status, schemaLayer.Detail = layerError, oneLine(err.Error())
		return pending, schemaLayer
	}
	if len(steps) > 0 {
		schemaLayer.Status = layerDrift
		schemaLayer.Detail = "the database differs from the schema its applied migrations build (changed outside a migration)"
		for _, s := range steps {
			schemaLayer.Items = append(schemaLayer.Items, s.ID+": "+s.Detail)
		}
		schemaLayer.Fix = "capture the change in a migration, or restore the database to what the migrations build"
		return pending, schemaLayer
	}
	at := "no migration applied yet"
	if state.LastApplied != "" {
		at = "as of " + state.LastApplied
	}
	schemaLayer.Status, schemaLayer.Detail = layerOK, "the database matches its migrations ("+at+")"
	return pending, schemaLayer
}

// readDatabase reads the application database within o.dbTimeout, so a
// database that never answers (a firewalled host) fails the layer instead of
// hanging the command. The driver's connect does not take a context, so the
// read runs aside and is abandoned on timeout; the process exits soon after.
func readDatabase(ctx context.Context, o checkOptions) (migrations.DatabaseState, error) {
	if o.dbTimeout <= 0 {
		return inspectDatabase(ctx, o.applyOptions())
	}
	ctx, cancel := context.WithTimeout(ctx, o.dbTimeout)
	defer cancel()
	type result struct {
		state migrations.DatabaseState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, err := inspectDatabase(ctx, o.applyOptions())
		done <- result{state, err}
	}()
	select {
	case r := <-done:
		return r.state, r.err
	case <-ctx.Done():
		return migrations.DatabaseState{}, fmt.Errorf("the database did not answer within %s (--db-timeout)", o.dbTimeout)
	}
}

func (o checkOptions) applyOptions() migrations.ApplyOptions {
	return migrations.ApplyOptions{
		WorkDir: o.dir.WorkDir, MigrationDir: o.dir.MigrationDir, AtlasBinary: o.dir.AtlasBinary, Database: o.database,
	}
}

// The checks' entry points; tests replace them.
var (
	inspectDatabase  = migrations.InspectDatabase
	generateContract = generate.Generate
	checkClientDrift = client.CheckDrift
)

func checkClient(ctx context.Context, o checkOptions) checkLayer {
	l := checkLayer{Name: "TypeScript client"}
	if o.openapiURL == "" {
		l.Status, l.Detail = layerSkipped, "pass --openapi-url to compare the committed client with a running app"
		return l
	}
	spec, err := fetchOpenAPI(ctx, o.openapiURL)
	if err != nil {
		l.Status, l.Detail = layerError, oneLine(err.Error())
		return l
	}
	var out bytes.Buffer
	err = checkClientDrift(ctx, client.DriftOptions{
		WorkDir: o.dir.WorkDir, SpecPath: client.DefaultSpecPath, OutDir: client.DefaultOutDir,
		SpecBytes: spec, Stdout: &out, Stderr: &out,
	})
	switch {
	case errors.Is(err, client.ErrDrift):
		l.Status, l.Detail, l.Fix = layerDrift, oneLine(err.Error()), "gombit client check --write --url "+o.openapiURL
		return l
	case err != nil:
		l.Status, l.Detail = layerError, oneLine(err.Error())
		if errors.Is(err, os.ErrNotExist) {
			l.Fix = "commit a first client: gombit client check --write --url " + o.openapiURL
		}
		return l
	}
	l.Status, l.Detail = layerOK, "the committed client matches "+o.openapiURL
	return l
}

func writeCheckReport(w io.Writer, r checkReport) {
	_, _ = fmt.Fprintf(w, "gombit db check (%s)\n\n", r.Driver)
	for _, l := range r.Layers {
		label := l.Status
		if l.Status == layerDrift || l.Status == layerError {
			label = strings.ToUpper(l.Status)
		}
		_, _ = fmt.Fprintf(w, "  %-8s %-20s %s\n", label, l.Name, l.Detail)
		for _, item := range l.Items {
			_, _ = fmt.Fprintf(w, "  %-8s %-20s - %s\n", "", "", item)
		}
		if l.Fix != "" && (l.Status == layerDrift || l.Status == layerError) {
			_, _ = fmt.Fprintf(w, "  %-8s %-20s fix: %s\n", "", "", l.Fix)
		}
	}
	if r.failed() {
		_, _ = fmt.Fprintln(w, "\nThe schema chain is inconsistent.")
		return
	}
	_, _ = fmt.Fprintln(w, "\nThe schema chain is consistent.")
}

// oneLine keeps an error on one report line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// registryDir is the migration directory the registry (models.json) lives
// in, resolved against the work dir the way the migrations package does.
func registryDir(o migrations.DirOptions) string {
	dir := o.MigrationDir
	if dir == "" {
		dir = filepath.Join("database", "migrations")
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	workDir := o.WorkDir
	if workDir == "" {
		workDir = "."
	}
	return filepath.Join(workDir, dir)
}
