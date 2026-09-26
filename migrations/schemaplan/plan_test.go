package schemaplan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ariga.io/atlas/sql/mysql"
	"ariga.io/atlas/sql/schema"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/migrations"
)

// The testdata fixtures are real `atlas schema inspect` output for one
// change set per dialect: products renames name to title, changes price's
// type, makes note NOT NULL, adds NOT NULL stock (and on PostgreSQL/MySQL a
// defaulted stock plus an undefaulted qty), adds a unique index on sku, and
// switches fk_owner from ON DELETE RESTRICT to CASCADE. SQLite also drops the
// legacy table.
func loadFixturePlan(t *testing.T, driver config.DatabaseDriver, dir string) []PlanStep {
	t.Helper()
	current := loadFixtureRealm(t, driver, filepath.Join("testdata", dir, "current.hcl"))
	desired := loadFixtureRealm(t, driver, filepath.Join("testdata", dir, "desired.hcl"))
	alignSchemas(current, desired)
	changes, err := differ(driver).RealmDiff(current, desired)
	if err != nil {
		t.Fatalf("RealmDiff: %v", err)
	}
	return classifyChanges(driver, changes)
}

func loadFixtureRealm(t *testing.T, driver config.DatabaseDriver, path string) *schema.Realm {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	realm := &schema.Realm{}
	if err := evalHCL(driver, data, realm); err != nil {
		t.Fatalf("eval %s: %v", path, err)
	}
	return realm
}

func stepsByID(steps []PlanStep) map[string]PlanStep {
	out := make(map[string]PlanStep, len(steps))
	for _, s := range steps {
		out[s.ID] = s
	}
	return out
}

func TestClassifyFixtureSQLite(t *testing.T) {
	steps := loadFixturePlan(t, config.DatabaseDriverSQLite, "sqlite")
	want := map[string]Severity{
		"drop_column:products.name":            SeverityDestructive,
		"widen_type:products.price":            SeverityReview, // integer -> text keeps every value
		"set_not_null:products.note":           SeverityUnsafe,
		"add_column:products.title":            SeveritySafe,
		"add_not_null:products.stock":          SeverityUnsafe,
		"add_unique:products.idx_sku":          SeverityUnsafe,
		"change_foreign_key:products.fk_owner": SeverityReview,
		"table_rebuild:products":               SeverityReview,
		"drop_table:legacy":                    SeverityDestructive,
	}
	assertSteps(t, steps, want)

	byID := stepsByID(steps)
	if hint := byID["drop_column:products.name"].Hint; !strings.Contains(hint, "--rename products.name:title") {
		t.Errorf("drop_column hint = %q, want the --rename suggestion for title", hint)
	}
	if d := byID["change_foreign_key:products.fk_owner"].Detail; !strings.Contains(d, "ON DELETE RESTRICT -> CASCADE") {
		t.Errorf("change_foreign_key detail = %q, want the delete action change", d)
	}
	// The table is rebuilt, so the NOT NULL column fails only with rows.
	if d := byID["add_not_null:products.stock"].Detail; !strings.Contains(d, "already has rows") {
		t.Errorf("add_not_null detail = %q, want the populated-table failure", d)
	}
}

func TestClassifyFixturePostgres(t *testing.T) {
	steps := loadFixturePlan(t, config.DatabaseDriverPostgres, "postgres")
	assertSteps(t, steps, map[string]Severity{
		"drop_column:products.name":            SeverityDestructive,
		"narrow_type:products.price":           SeverityDestructive, // bigint -> integer
		"narrow_type:products.note":            SeverityDestructive, // text -> varchar(100)
		"set_not_null:products.note":           SeverityUnsafe,
		"add_column:products.title":            SeveritySafe,
		"add_column:products.stock":            SeveritySafe, // NOT NULL DEFAULT 0
		"add_not_null:products.qty":            SeverityUnsafe,
		"add_unique:products.idx_sku":          SeverityUnsafe,
		"change_foreign_key:products.fk_owner": SeverityReview,
	})
}

func TestClassifyFixtureMySQL(t *testing.T) {
	steps := loadFixturePlan(t, config.DatabaseDriverMySQL, "mysql")
	assertSteps(t, steps, map[string]Severity{
		"drop_column:products.name":            SeverityDestructive,
		"narrow_type:products.price":           SeverityDestructive, // bigint -> int
		"narrow_type:products.note":            SeverityDestructive, // varchar(255) -> varchar(100)
		"set_not_null:products.note":           SeverityUnsafe,
		"add_column:products.title":            SeveritySafe,
		"add_column:products.stock":            SeveritySafe,
		"add_not_null:products.qty":            SeverityUnsafe,
		"add_unique:products.idx_sku":          SeverityUnsafe,
		"change_foreign_key:products.fk_owner": SeverityReview,
	})
}

func assertSteps(t *testing.T, steps []PlanStep, want map[string]Severity) {
	t.Helper()
	seen := map[string]bool{}
	for _, s := range steps {
		if seen[s.ID] {
			t.Errorf("duplicate step ID %s", s.ID)
		}
		seen[s.ID] = true
	}
	got := stepsByID(steps)
	for id, sev := range want {
		s, ok := got[id]
		if !ok {
			t.Errorf("missing step %s; got %v", id, stepIDs(steps))
			continue
		}
		if s.Severity != sev {
			t.Errorf("step %s severity = %s, want %s", id, s.Severity, sev)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected step %s (%s): %s", id, got[id].Severity, got[id].Detail)
		}
	}
}

func stepIDs(steps []PlanStep) []string {
	ids := make([]string, 0, len(steps))
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestClassifyTableChanges(t *testing.T) {
	const current = `
table "items" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "code" {
    null = true
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {}
`
	cases := []struct {
		name    string
		desired string
		want    map[string]Severity
	}{
		{
			name: "nullable column added in place",
			desired: `
table "items" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "code" {
    null = true
    type = text
  }
  column "note" {
    null = true
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {}
`,
			want: map[string]Severity{"add_column:items.note": SeveritySafe},
		},
		{
			name: "unique index over a new nullable column cannot collide",
			desired: `
table "items" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "code" {
    null = true
    type = text
  }
  column "slug" {
    null = true
    type = text
  }
  primary_key {
    columns = [column.id]
  }
  index "idx_slug" {
    unique  = true
    columns = [column.slug]
  }
}
schema "main" {}
`,
			want: map[string]Severity{
				"add_column:items.slug":     SeveritySafe,
				"add_unique:items.idx_slug": SeveritySafe,
				// A new column that carries an index is not an in-place ALTER.
				"table_rebuild:items": SeverityReview,
			},
		},
		{
			name: "NOT NULL column added in place is rejected by SQLite",
			desired: `
table "items" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "code" {
    null = true
    type = text
  }
  column "qty" {
    null = false
    type = integer
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {}
`,
			want: map[string]Severity{"add_not_null:items.qty": SeverityUnsafe},
		},
		{
			name: "check constraint on existing rows",
			desired: `
table "items" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "code" {
    null = true
    type = text
  }
  primary_key {
    columns = [column.id]
  }
  check "chk_code" {
    expr = "(length(code) > 2)"
  }
}
schema "main" {}
`,
			want: map[string]Severity{
				"add_check:items.chk_code": SeverityUnsafe,
				"table_rebuild:items":      SeverityReview,
			},
		},
		{
			name: "dropping the table",
			desired: `
schema "main" {}
`,
			want: map[string]Severity{"drop_table:items": SeverityDestructive},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur, want := &schema.Realm{}, &schema.Realm{}
			if err := evalHCL(config.DatabaseDriverSQLite, []byte(current), cur); err != nil {
				t.Fatal(err)
			}
			if err := evalHCL(config.DatabaseDriverSQLite, []byte(tc.desired), want); err != nil {
				t.Fatal(err)
			}
			changes, err := differ(config.DatabaseDriverSQLite).RealmDiff(cur, want)
			if err != nil {
				t.Fatal(err)
			}
			steps := classifyChanges(config.DatabaseDriverSQLite, changes)
			assertSteps(t, steps, tc.want)
			if tc.name == "NOT NULL column added in place is rejected by SQLite" {
				if d := stepsByID(steps)["add_not_null:items.qty"].Detail; !strings.Contains(d, "even on an empty table") {
					t.Errorf("detail = %q, want the SQLite in-place rejection", d)
				}
			}
		})
	}
}

func TestTypeDirection(t *testing.T) {
	pg := config.DatabaseDriverPostgres
	cases := []struct {
		name     string
		from, to schema.Type
		want     typeDir
	}{
		{"int to bigint", &schema.IntegerType{T: "integer"}, &schema.IntegerType{T: "bigint"}, typeWiden},
		{"bigint to smallint", &schema.IntegerType{T: "bigint"}, &schema.IntegerType{T: "smallint"}, typeNarrow},
		{"signedness flip", &schema.IntegerType{T: "bigint"}, &schema.IntegerType{T: "bigint", Unsigned: true}, typeUnknown},
		{"varchar grows", &schema.StringType{T: "varchar", Size: 100}, &schema.StringType{T: "varchar", Size: 255}, typeWiden},
		{"varchar shrinks", &schema.StringType{T: "varchar", Size: 255}, &schema.StringType{T: "varchar", Size: 100}, typeNarrow},
		{"varchar to text", &schema.StringType{T: "character varying", Size: 255}, &schema.StringType{T: "text"}, typeWiden},
		{"text to varchar", &schema.StringType{T: "text"}, &schema.StringType{T: "character varying", Size: 255}, typeNarrow},
		{"decimal grows", &schema.DecimalType{T: "decimal", Precision: 10, Scale: 2}, &schema.DecimalType{T: "decimal", Precision: 19, Scale: 4}, typeWiden},
		{"decimal loses scale", &schema.DecimalType{T: "decimal", Precision: 19, Scale: 4}, &schema.DecimalType{T: "decimal", Precision: 19, Scale: 2}, typeNarrow},
		{"int into small decimal", &schema.IntegerType{T: "bigint"}, &schema.DecimalType{T: "decimal", Precision: 10, Scale: 2}, typeNarrow},
		{"int into text", &schema.IntegerType{T: "bigint"}, &schema.StringType{T: "text"}, typeWiden},
		{"float to double", &schema.FloatType{T: "real"}, &schema.FloatType{T: "double precision"}, typeWiden},
		{"double to real", &schema.FloatType{T: "double precision"}, &schema.FloatType{T: "real"}, typeNarrow},
		{"text to integer", &schema.StringType{T: "text"}, &schema.IntegerType{T: "bigint"}, typeUnknown},
		{"bool to text", &schema.BoolType{T: "boolean"}, &schema.StringType{T: "text"}, typeWiden},
	}
	for _, tc := range cases {
		if got := typeDirection(pg, tc.from, tc.to); got != tc.want {
			t.Errorf("%s: typeDirection = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSchemaPlanAcknowledge(t *testing.T) {
	plan := SchemaPlan{Steps: []PlanStep{
		newStep(StepDropColumn, SeverityDestructive, "products", "name", ""),
		newStep(StepAddNotNull, SeverityUnsafe, "products", "stock", ""),
		newStep(StepAddNotNull, SeverityUnsafe, "orders", "qty", ""),
		newStep(StepDropTable, SeverityDestructive, "legacy", "", ""),
		newStep(StepChangeForeignKey, SeverityReview, "products", "fk_owner", ""),
	}}
	if got := len(plan.Unacknowledged()); got != 4 {
		t.Fatalf("Unacknowledged() = %d steps, want the 4 destructive/unsafe ones", got)
	}
	plan.Acknowledge([]string{"drop_column:products.name", "add_not_null"})
	pending := plan.Unacknowledged()
	if len(pending) != 1 || pending[0].ID != "drop_table:legacy" {
		t.Fatalf("Unacknowledged() after --allow = %v, want only drop_table:legacy", stepIDs(pending))
	}

	if unmatched := plan.Acknowledge([]string{"drop_column:products.name", "drop_column", "drop_colum:products.name"}); len(unmatched) != 1 || unmatched[0] != "drop_colum:products.name" {
		t.Fatalf("Acknowledge() unmatched = %v, want only the typo", unmatched)
	}

	// --forget-model acknowledges its own model's table under GORM's default
	// name, and nothing else: a hand-written table's drop still needs --allow.
	forget := SchemaPlan{
		Steps: []PlanStep{
			newStep(StepDropTable, SeverityDestructive, "legacy_widgets", "", ""),
			newStep(StepDropTable, SeverityDestructive, "audit_log", "", ""),
		},
		forgottenTables: defaultTableNames([]migrations.Model{{ImportPath: "example.com/app/internal/widget", TypeName: "LegacyWidget"}}),
	}
	forget.Acknowledge(nil)
	pending = forget.Unacknowledged()
	if len(pending) != 1 || pending[0].ID != "drop_table:audit_log" {
		t.Fatalf("Unacknowledged() after --forget-model = %v, want only drop_table:audit_log", stepIDs(pending))
	}
}

func TestWritePlan(t *testing.T) {
	var buf bytes.Buffer
	WritePlan(&buf, SchemaPlan{Driver: config.DatabaseDriverSQLite})
	if !strings.Contains(buf.String(), "No schema changes") {
		t.Fatalf("empty plan = %q", buf.String())
	}

	step := newStep(StepDropColumn, SeverityDestructive, "products", "name", "Drops column products.name and the data in it.")
	step.Hint = "line one\nline two"
	buf.Reset()
	WritePlan(&buf, SchemaPlan{Driver: config.DatabaseDriverSQLite, Steps: []PlanStep{step, newStep(StepAddIndex, SeveritySafe, "products", "idx", "Adds index idx.")}})
	out := buf.String()
	for _, want := range []string{"DESTRUCTIVE  drop_column:products.name", "line two", "safe", "--allow drop_column:products.name"} {
		if !strings.Contains(out, want) {
			t.Errorf("WritePlan output missing %q:\n%s", want, out)
		}
	}
}

func TestClassifyModifyColumnTypeChanges(t *testing.T) {
	col := func(typ schema.Type, null bool) *schema.Column {
		return &schema.Column{Name: "code", Type: &schema.ColumnType{Type: typ, Null: null}}
	}
	change := &schema.ModifyColumn{From: col(&schema.StringType{T: "text"}, true), To: col(&schema.IntegerType{T: "bigint"}, true), Change: schema.ChangeType}

	pg := stepsByID(classifyModifyColumn(config.DatabaseDriverPostgres, schema.NewTable("items"), change))
	if s := pg["change_type:items.code"]; s.Severity != SeverityUnsafe {
		t.Errorf("postgres text -> bigint = %+v, want an unsafe change_type", s)
	}
	lite := stepsByID(classifyModifyColumn(config.DatabaseDriverSQLite, schema.NewTable("items"), change))
	if s := lite["change_type:items.code"]; s.Severity != SeverityReview {
		t.Errorf("sqlite text -> bigint = %+v, want a review change_type (affinity keeps values)", s)
	}
	shrink := &schema.ModifyColumn{From: col(&schema.StringType{T: "varchar", Size: 255}, true), To: col(&schema.StringType{T: "varchar", Size: 10}, true), Change: schema.ChangeType}
	if s := stepsByID(classifyModifyColumn(config.DatabaseDriverSQLite, schema.NewTable("items"), shrink))["change_type:items.code"]; s.Severity != SeverityReview {
		t.Errorf("sqlite varchar(255) -> varchar(10) = %+v, want review: SQLite does not truncate", s)
	}
}

func TestClassifyForeignKeys(t *testing.T) {
	parent := schema.NewTable("owners").AddColumns(schema.NewIntColumn("id", "integer"))
	child := schema.NewTable("items").AddColumns(schema.NewIntColumn("owner_id", "integer"), schema.NewNullIntColumn("new_owner_id", "integer"))
	existing := schema.NewForeignKey("fk_owner").AddColumns(child.Columns[0]).SetRefTable(parent).AddRefColumns(parent.Columns[0])
	fresh := schema.NewForeignKey("fk_new_owner").AddColumns(child.Columns[1]).SetRefTable(parent).AddRefColumns(parent.Columns[0])
	steps := stepsByID(classifyTable(config.DatabaseDriverPostgres, &schema.ModifyTable{T: child, Changes: []schema.Change{
		&schema.AddColumn{C: child.Columns[1]},
		&schema.AddForeignKey{F: existing},
		&schema.AddForeignKey{F: fresh},
		// A different delete rule, so this is a real drop, not fk_owner renamed.
		&schema.DropForeignKey{F: schema.NewForeignKey("fk_old").AddColumns(child.Columns[0]).SetRefTable(parent).AddRefColumns(parent.Columns[0]).SetOnDelete(schema.Cascade)},
	}}))
	if s := steps["add_foreign_key:items.fk_owner"]; s.Severity != SeverityUnsafe {
		t.Errorf("FK over an existing column = %+v, want unsafe (orphans fail it)", s)
	}
	if s := steps["add_foreign_key:items.fk_new_owner"]; s.Severity != SeveritySafe {
		t.Errorf("FK over a new nullable column = %+v, want safe (every row holds NULL)", s)
	}
	if s := steps["drop_foreign_key:items.fk_old"]; s.Severity != SeverityReview {
		t.Errorf("dropped FK = %+v, want review", s)
	}
}

func fixtureInspection(t *testing.T, driver config.DatabaseDriver, dir string) migrations.Inspection {
	t.Helper()
	read := func(file string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", dir, file)) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return migrations.Inspection{Driver: driver, Current: read("current.hcl"), Desired: read("desired.hcl")}
}

func TestBuildEmptyCurrentListsNewTables(t *testing.T) {
	in := fixtureInspection(t, config.DatabaseDriverPostgres, "postgres")
	in.Current = nil
	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	// No spurious schema change: the empty side takes the public schema's attrs.
	assertSteps(t, plan.Steps, map[string]Severity{"add_table:products": SeveritySafe, "add_table:users": SeveritySafe})
}

func TestGateRefusesUnacknowledgedSteps(t *testing.T) {
	in := fixtureInspection(t, config.DatabaseDriverSQLite, "sqlite")
	in.NewModels = []migrations.Model{{ImportPath: "example.com/app/internal/order", TypeName: "Order"}}
	in.ForgetModels = []migrations.Model{{ImportPath: "example.com/app/internal/legacy", TypeName: "Legacy"}}
	var stderr bytes.Buffer
	_, err := Gate([]string{"drop_colum:products.name"}, &stderr)(context.Background(), "reshape_products", in)
	if err == nil || !strings.Contains(err.Error(), "need acknowledgement") {
		t.Fatalf("Gate() error = %v, want the refusal", err)
	}
	// A refused run saves no registry: the retry command must name the models
	// it was adding and forgetting, and acknowledge every pending step.
	want := "gombit db makemigrations reshape_products --model example.com/app/internal/order.Order --forget-model example.com/app/internal/legacy.Legacy --allow drop_column:products.name"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Gate() error = %v, want the retry command %q", err, want)
	}
	for _, id := range []string{"set_not_null:products.note", "add_not_null:products.stock", "add_unique:products.idx_sku", "drop_table:legacy"} {
		if !strings.Contains(err.Error(), "--allow "+id) {
			t.Errorf("retry command missing --allow %s: %v", id, err)
		}
	}
	for _, want := range []string{"warning: --allow drop_colum:products.name matched no change", "DESTRUCTIVE  drop_column:products.name", "--rename products.name:title", "--allow add_not_null:products.stock"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestGatePassesAcknowledgedSteps(t *testing.T) {
	in := fixtureInspection(t, config.DatabaseDriverSQLite, "sqlite")
	in.ForgetModels = []migrations.Model{{ImportPath: "example.com/app/internal/legacy", TypeName: "Legacy"}}
	allow := []string{"drop_column:products.name", "set_not_null", "add_not_null", "add_unique"}
	// drop_table:legacy is covered by forgetting the Legacy model (GORM names
	// its table "legacies", not "legacy"), so it is still pending here.
	if _, err := Gate(allow, io.Discard)(context.Background(), "reshape", in); err == nil || !strings.Contains(err.Error(), "drop_table:legacy") {
		t.Fatalf("Gate() error = %v, want drop_table:legacy still pending", err)
	}
	ack, err := Gate(append(allow, "drop_table:legacy"), io.Discard)(context.Background(), "reshape", in)
	if err != nil {
		t.Fatalf("Gate() error = %v, want nil once every step is allowed", err)
	}
	// Every destructive or unsafe step is returned for the migration to record.
	for _, id := range []string{"drop_column:products.name", "set_not_null:products.note", "add_not_null:products.stock", "add_unique:products.idx_sku", "drop_table:legacy"} {
		if !slices.Contains(ack, id) {
			t.Errorf("Gate() acknowledged %v, missing %s", ack, id)
		}
	}
}

// TestPlanAndGateAtlasCLISQLiteWhenAvailable runs the real loader and Atlas.
// The migration directory builds products with a title column and no price;
// the Product model has name and price, both NOT NULL.
func TestPlanAndGateAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite plan test")
		}
	}
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	migrationDir := t.TempDir()
	sql := "CREATE TABLE `products` (`id` integer NULL PRIMARY KEY AUTOINCREMENT, `created_at` datetime NULL, `updated_at` datetime NULL, `deleted_at` datetime NULL, `title` varchar(120) NOT NULL);\n" +
		"CREATE INDEX `idx_products_deleted_at` ON `products` (`deleted_at`);\n"
	if err := os.WriteFile(filepath.Join(migrationDir, "20260101000000_create_products.sql"), []byte(sql), 0o600); err != nil {
		t.Fatal(err)
	}
	product := migrations.Model{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Product"}
	if err := migrations.SaveRegistry(migrationDir, []migrations.Model{product}); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Hash(ctx, migrations.ApplyOptions{WorkDir: root, MigrationDir: migrationDir, AtlasBinary: atlasBin, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatalf("Hash() error = %v", err)
	}

	in, err := migrations.Inspect(ctx, migrations.InspectOptions{WorkDir: root, Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assertSteps(t, plan.Steps, map[string]Severity{
		"drop_column:products.title":  SeverityDestructive,
		"add_not_null:products.name":  SeverityUnsafe,
		"add_not_null:products.price": SeverityUnsafe,
		"table_rebuild:products":      SeverityReview,
	})
	if hint := stepsByID(plan.Steps)["drop_column:products.title"].Hint; !strings.Contains(hint, "--rename products.title:name") {
		t.Errorf("drop_column hint = %q, want the rename suggestion", hint)
	}

	opts := migrations.Options{WorkDir: root, Name: "reshape", Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Stdout: io.Discard, Stderr: io.Discard}
	opts.Gate = Gate(nil, io.Discard)
	if err := migrations.MakeMigrations(ctx, opts); err == nil || !strings.Contains(err.Error(), "need acknowledgement") {
		t.Fatalf("MakeMigrations() error = %v, want the acknowledgement refusal", err)
	}
	if files, _ := filepath.Glob(filepath.Join(migrationDir, "*.sql")); len(files) != 1 {
		t.Fatalf("migration files = %v, want no new migration", files)
	}
	opts.Gate = Gate([]string{"drop_column:products.title", "add_not_null"}, io.Discard)
	if err := migrations.MakeMigrations(ctx, opts); err != nil {
		t.Fatalf("MakeMigrations(--allow) error = %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(migrationDir, "*.sql")); len(files) != 2 {
		t.Fatalf("migration files = %v, want the acknowledged migration", files)
	}
}

// TestDefaultedNewColumnIsNotNullFilled is the regression test for a unique
// index or foreign key over a new nullable column: it can fail only when the
// column has a default, which Atlas writes into every existing row before the
// constraint is created.
func TestDefaultedNewColumnIsNotNullFilled(t *testing.T) {
	type shape struct {
		driver  config.DatabaseDriver
		schema  string
		idType  string
		strType string
		strDef  string
	}
	shapes := []shape{
		{config.DatabaseDriverSQLite, "main", "integer", "text", `"pending"`},
		{config.DatabaseDriverPostgres, "public", "bigint", "text", `"pending"`},
		{config.DatabaseDriverMySQL, "dev", "bigint", "varchar(32)", `"pending"`},
	}
	realm := func(t *testing.T, s shape, withSKU, skuDefault bool, withOwner, ownerDefault bool) *schema.Realm {
		t.Helper()
		var b strings.Builder
		fmt.Fprintf(&b, "table \"owners\" {\n  schema = schema.%s\n  column \"id\" {\n    null = false\n    type = %s\n  }\n  primary_key {\n    columns = [column.id]\n  }\n}\n", s.schema, s.idType)
		fmt.Fprintf(&b, "table \"products\" {\n  schema = schema.%s\n  column \"id\" {\n    null = false\n    type = %s\n  }\n", s.schema, s.idType)
		if withSKU {
			def := ""
			if skuDefault {
				def = "    default = " + s.strDef + "\n"
			}
			fmt.Fprintf(&b, "  column \"sku\" {\n    null = true\n    type = %s\n%s  }\n", s.strType, def)
		}
		if withOwner {
			def := ""
			if ownerDefault {
				def = "    default = 1\n"
			}
			fmt.Fprintf(&b, "  column \"owner_id\" {\n    null = true\n    type = %s\n%s  }\n", s.idType, def)
		}
		b.WriteString("  primary_key {\n    columns = [column.id]\n  }\n")
		if withOwner {
			b.WriteString("  foreign_key \"fk_owner\" {\n    columns     = [column.owner_id]\n    ref_columns = [table.owners.column.id]\n    on_update   = NO_ACTION\n    on_delete   = RESTRICT\n  }\n")
		}
		if withSKU {
			b.WriteString("  index \"idx_sku\" {\n    unique  = true\n    columns = [column.sku]\n  }\n")
		}
		fmt.Fprintf(&b, "}\nschema %q {}\n", s.schema)
		r := &schema.Realm{}
		if err := evalHCL(s.driver, []byte(b.String()), r); err != nil {
			t.Fatalf("eval %s HCL: %v\n%s", s.driver, err, b.String())
		}
		return r
	}
	for _, s := range shapes {
		for _, withDefault := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/default=%v", s.driver, withDefault), func(t *testing.T) {
				cur := realm(t, s, false, false, false, false)
				want := realm(t, s, true, withDefault, true, withDefault)
				changes, err := differ(s.driver).RealmDiff(cur, want)
				if err != nil {
					t.Fatal(err)
				}
				steps := stepsByID(classifyChanges(s.driver, changes))
				expect := SeveritySafe
				if withDefault {
					expect = SeverityUnsafe
				}
				if got := steps["add_unique:products.idx_sku"]; got.Severity != expect {
					t.Errorf("unique index over a new nullable column (default=%v) = %+v, want %s", withDefault, got, expect)
				}
				if got := steps["add_foreign_key:products.fk_owner"]; got.Severity != expect {
					t.Errorf("foreign key over a new nullable column (default=%v) = %+v, want %s", withDefault, got, expect)
				}
			})
		}
	}
}

func TestNullDefault(t *testing.T) {
	for _, tc := range []struct {
		d    schema.Expr
		want bool
	}{
		{nil, true},
		{&schema.Literal{V: "NULL"}, true},
		{&schema.RawExpr{X: "null"}, true},
		{&schema.Literal{V: "'pending'"}, false},
		{&schema.Literal{V: "0"}, false},
		{&schema.RawExpr{X: "CURRENT_TIMESTAMP"}, false},
	} {
		if got := nullDefault(tc.d); got != tc.want {
			t.Errorf("nullDefault(%#v) = %v, want %v", tc.d, got, tc.want)
		}
	}
}

// TestRecreatedConstraintsAreRechecked diffs same-named constraints whose key
// changes. Atlas reports them as ModifyIndex / ModifyForeignKey, and the
// migration re-creates them against the existing rows.
func TestRecreatedConstraintsAreRechecked(t *testing.T) {
	type dialect struct {
		driver  config.DatabaseDriver
		schema  string
		idType  string
		strType string
	}
	dialects := []dialect{
		{config.DatabaseDriverSQLite, "main", "integer", "text"},
		{config.DatabaseDriverPostgres, "public", "bigint", "text"},
		{config.DatabaseDriverMySQL, "dev", "bigint", "varchar(64)"},
	}
	type state struct {
		indexCols    string // columns of idx_member
		indexComment string
		fkCols       string // columns of fk_owner
		fkRef        string // referenced column of fk_owner
		fkDelete     string
		newOwner     bool // add a nullable owner2_id with no default
	}
	base := state{indexCols: "column.org_id, column.email", fkCols: "column.owner_id", fkRef: "table.users.column.id", fkDelete: "RESTRICT"}
	build := func(t *testing.T, d dialect, st state) *schema.Realm {
		t.Helper()
		var b strings.Builder
		fmt.Fprintf(&b, "table \"users\" {\n  schema = schema.%s\n  column \"id\" {\n    null = false\n    type = %s\n  }\n  column \"code\" {\n    null = false\n    type = %s\n  }\n  primary_key {\n    columns = [column.id]\n  }\n  index \"idx_code\" {\n    unique  = true\n    columns = [column.code]\n  }\n}\n", d.schema, d.idType, d.strType)
		fmt.Fprintf(&b, "table \"members\" {\n  schema = schema.%s\n", d.schema)
		fmt.Fprintf(&b, "  column \"id\" {\n    null = false\n    type = %s\n  }\n", d.idType)
		fmt.Fprintf(&b, "  column \"org_id\" {\n    null = false\n    type = %s\n  }\n", d.idType)
		fmt.Fprintf(&b, "  column \"email\" {\n    null = false\n    type = %s\n  }\n", d.strType)
		fmt.Fprintf(&b, "  column \"name\" {\n    null = false\n    type = %s\n  }\n", d.strType)
		refType := d.idType
		if st.fkRef == "table.users.column.code" {
			refType = d.strType
		}
		fmt.Fprintf(&b, "  column \"owner_id\" {\n    null = true\n    type = %s\n  }\n", refType)
		if st.newOwner {
			fmt.Fprintf(&b, "  column \"owner2_id\" {\n    null = true\n    type = %s\n  }\n", d.idType)
		}
		b.WriteString("  primary_key {\n    columns = [column.id]\n  }\n")
		fmt.Fprintf(&b, "  foreign_key \"fk_owner\" {\n    columns     = [%s]\n    ref_columns = [%s]\n    on_update   = NO_ACTION\n    on_delete   = %s\n  }\n", st.fkCols, st.fkRef, st.fkDelete)
		comment := ""
		if st.indexComment != "" {
			comment = fmt.Sprintf("    comment = %q\n", st.indexComment)
		}
		fmt.Fprintf(&b, "  index \"idx_member\" {\n    unique  = true\n    columns = [%s]\n%s  }\n}\n", st.indexCols, comment)
		fmt.Fprintf(&b, "schema %q {}\n", d.schema)
		r := &schema.Realm{}
		if err := evalHCL(d.driver, []byte(b.String()), r); err != nil {
			t.Fatalf("eval %s HCL: %v\n%s", d.driver, err, b.String())
		}
		return r
	}
	cases := []struct {
		name   string
		to     func(state) state
		id     string
		want   Severity
		sqlite bool // SQLite HCL has no index comments
	}{
		{"unique index columns change", func(s state) state { s.indexCols = "column.org_id, column.name"; return s }, "add_unique:members.idx_member", SeverityUnsafe, true},
		{"unique index comment only", func(s state) state { s.indexComment = "members per org"; return s }, "add_index:members.idx_member", SeveritySafe, false},
		{"foreign key retargeted", func(s state) state { s.fkRef = "table.users.column.code"; return s }, "add_foreign_key:members.fk_owner", SeverityUnsafe, true},
		{"foreign key moved to a new null-filled column", func(s state) state { s.fkCols = "column.owner2_id"; s.newOwner = true; return s }, "add_foreign_key:members.fk_owner", SeveritySafe, true},
		{"foreign key delete action only", func(s state) state { s.fkDelete = "CASCADE"; return s }, "change_foreign_key:members.fk_owner", SeverityReview, true},
	}
	for _, d := range dialects {
		for _, tc := range cases {
			if d.driver == config.DatabaseDriverSQLite && !tc.sqlite {
				continue
			}
			t.Run(string(d.driver)+"/"+tc.name, func(t *testing.T) {
				changes, err := differ(d.driver).RealmDiff(build(t, d, base), build(t, d, tc.to(base)))
				if err != nil {
					t.Fatal(err)
				}
				steps := classifyChanges(d.driver, changes)
				got, ok := stepsByID(steps)[tc.id]
				if !ok || got.Severity != tc.want {
					t.Fatalf("step %s = %+v (present %v), want %s; steps %v", tc.id, got, ok, tc.want, stepIDs(steps))
				}
			})
		}
	}
}

func TestPrimaryKeyChanges(t *testing.T) {
	tbl := schema.NewTable("items").AddColumns(schema.NewIntColumn("id", "integer"))
	pk := schema.NewPrimaryKey(tbl.Columns...)
	for _, tc := range []struct {
		change schema.Change
		want   Severity
	}{
		{&schema.AddPrimaryKey{P: pk}, SeverityUnsafe},
		{&schema.ModifyPrimaryKey{From: pk, To: pk}, SeverityUnsafe},
		{&schema.DropPrimaryKey{P: pk}, SeverityReview},
	} {
		steps := classifyTable(config.DatabaseDriverPostgres, &schema.ModifyTable{T: tbl, Changes: []schema.Change{tc.change}})
		if len(steps) != 1 || steps[0].Severity != tc.want {
			t.Errorf("%T = %+v, want one %s step", tc.change, steps, tc.want)
		}
	}
}

func TestFloatWidthPerDialect(t *testing.T) {
	p := func(v int) int { return v }
	cases := []struct {
		name     string
		driver   config.DatabaseDriver
		from, to *schema.FloatType
		want     typeDir
	}{
		// GORM on MySQL: float64 is double, float32 is float.
		{"mysql double to float", config.DatabaseDriverMySQL, &schema.FloatType{T: "double"}, &schema.FloatType{T: "float"}, typeNarrow},
		{"mysql float to double", config.DatabaseDriverMySQL, &schema.FloatType{T: "float"}, &schema.FloatType{T: "double"}, typeWiden},
		{"mysql float(24) is double", config.DatabaseDriverMySQL, &schema.FloatType{T: "double"}, &schema.FloatType{T: "float", Precision: p(24)}, typeWiden},
		{"mysql float(23) is single", config.DatabaseDriverMySQL, &schema.FloatType{T: "double"}, &schema.FloatType{T: "float", Precision: p(23)}, typeNarrow},
		{"postgres float with no p is double", config.DatabaseDriverPostgres, &schema.FloatType{T: "double precision"}, &schema.FloatType{T: "float"}, typeWiden},
		{"postgres float(24) is real", config.DatabaseDriverPostgres, &schema.FloatType{T: "double precision"}, &schema.FloatType{T: "float", Precision: p(24)}, typeNarrow},
	}
	for _, tc := range cases {
		if got := typeDirection(tc.driver, tc.from, tc.to); got != tc.want {
			t.Errorf("%s: typeDirection = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// mysqlColumnPlan diffs a MySQL table whose one column changes from the
// "from" attribute lines to the "to" ones, as inspected HCL.
func mysqlColumnPlan(t *testing.T, from, to string, unique bool) map[string]PlanStep {
	t.Helper()
	build := func(attrs string) *schema.Realm {
		idx := ""
		if unique {
			idx = "  index \"idx_value\" {\n    unique  = true\n    columns = [column.value]\n  }\n"
		}
		hcl := "table \"readings\" {\n  schema = schema.dev\n  column \"id\" {\n    null = false\n    type = bigint\n  }\n  column \"value\" {\n    null = false\n" + attrs + "  }\n  primary_key {\n    columns = [column.id]\n  }\n" + idx + "}\nschema \"dev\" {\n  charset = \"utf8mb4\"\n  collate = \"utf8mb4_0900_ai_ci\"\n}\n"
		r := &schema.Realm{}
		if err := evalHCL(config.DatabaseDriverMySQL, []byte(hcl), r); err != nil {
			t.Fatalf("eval: %v\n%s", err, hcl)
		}
		return r
	}
	changes, err := differ(config.DatabaseDriverMySQL).RealmDiff(build(from), build(to))
	if err != nil {
		t.Fatal(err)
	}
	steps := classifyChanges(config.DatabaseDriverMySQL, changes)
	assertNoDuplicateIDs(t, steps)
	return stepsByID(steps)
}

func assertNoDuplicateIDs(t *testing.T, steps []PlanStep) {
	t.Helper()
	seen := map[string]bool{}
	for _, s := range steps {
		if seen[s.ID] {
			t.Errorf("duplicate step ID %s", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestMySQLInspectedColumnChanges(t *testing.T) {
	const text = "    type = varchar(64)\n"
	cases := []struct {
		name     string
		from, to string
		unique   bool
		id       string
		want     Severity
	}{
		{"float64 to float32", "    type = double\n", "    type = float\n", false, "narrow_type:readings.value", SeverityDestructive},
		{"charset to ascii", text + "    charset = \"utf8mb4\"\n    collate = \"utf8mb4_bin\"\n", text + "    charset = \"ascii\"\n    collate = \"ascii_bin\"\n", false, "change_charset:readings.value", SeverityUnsafe},
		{"charset to utf8mb4", text + "    charset = \"latin1\"\n    collate = \"latin1_bin\"\n", text + "    charset = \"utf8mb4\"\n    collate = \"utf8mb4_bin\"\n", false, "change_charset:readings.value", SeverityReview},
		{"collation in a unique key", text + "    collate = \"utf8mb4_bin\"\n", text + "    collate = \"utf8mb4_0900_ai_ci\"\n", true, "change_collation:readings.value", SeverityUnsafe},
		{"collation on a plain column", text + "    collate = \"utf8mb4_bin\"\n", text + "    collate = \"utf8mb4_0900_ai_ci\"\n", false, "change_collation:readings.value", SeverityReview},
		{"comment only", text + "    comment = \"old\"\n", text + "    comment = \"new\"\n", false, "change_comment:readings.value", SeveritySafe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := mysqlColumnPlan(t, tc.from, tc.to, tc.unique)
			got, ok := steps[tc.id]
			if !ok || got.Severity != tc.want {
				t.Fatalf("step %s = %+v (present %v), want %s; steps %v", tc.id, got, ok, tc.want, steps)
			}
			if tc.name == "comment only" && len(steps) != 1 {
				t.Fatalf("comment-only change produced %v, want only change_comment", steps)
			}
		})
	}
}

// unknownChange stands in for a schema change the classifier has no arm for.
type unknownChange struct{ schema.Change }

func TestUnclassifiedChangesFailClosed(t *testing.T) {
	tbl := schema.NewTable("items")
	steps := classifyTable(config.DatabaseDriverMySQL, &schema.ModifyTable{T: tbl, Changes: []schema.Change{
		&schema.ModifyAttr{From: &schema.Comment{Text: "old"}, To: &schema.Comment{Text: "new"}},
		unknownChange{},
	}})
	if len(steps) != 2 || steps[0].Severity != SeveritySafe || steps[1].Severity != SeverityUnsafe || steps[1].Code != StepOther {
		t.Fatalf("steps = %+v, want a safe table option and an unsafe unclassified change", steps)
	}
	col := &schema.Column{Name: "n", Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}}}
	attr := classifyModifyColumn(config.DatabaseDriverMySQL, tbl, &schema.ModifyColumn{From: col, To: col, Change: schema.ChangeAttr})
	if len(attr) != 1 || attr[0].Severity != SeverityUnsafe {
		t.Fatalf("unrecognized column attribute change = %+v, want one unsafe step", attr)
	}
	top := classifyChanges(config.DatabaseDriverMySQL, []schema.Change{&schema.AddSchema{S: schema.New("other")}})
	if len(top) != 1 || top[0].Severity != SeverityUnsafe {
		t.Fatalf("unrecognized top-level change = %+v, want one unsafe step", top)
	}
}

func TestStringCapacityPerDialect(t *testing.T) {
	my, pg := config.DatabaseDriverMySQL, config.DatabaseDriverPostgres
	str := func(name string, size int) *schema.StringType { return &schema.StringType{T: name, Size: size} }
	cases := []struct {
		name     string
		driver   config.DatabaseDriver
		from, to *schema.StringType
		want     typeDir
	}{
		// Atlas stores no size for MySQL's text types; each has a byte cap.
		{"mysql varchar(255) to tinytext", my, str("varchar", 255), str("tinytext", 0), typeNarrow},
		{"mysql varchar(60) to tinytext", my, str("varchar", 60), str("tinytext", 0), typeWiden},
		{"mysql varchar(255) to text", my, str("varchar", 255), str("text", 0), typeWiden},
		{"mysql varchar(20000) to text", my, str("varchar", 20000), str("text", 0), typeNarrow},
		{"mysql tinytext to varchar(255)", my, str("tinytext", 0), str("varchar", 255), typeWiden},
		{"mysql tinytext to varchar(100)", my, str("tinytext", 0), str("varchar", 100), typeNarrow},
		{"mysql text to tinytext", my, str("text", 0), str("tinytext", 0), typeNarrow},
		{"mysql mediumtext to text", my, str("mediumtext", 0), str("text", 0), typeNarrow},
		{"mysql text to longtext", my, str("text", 0), str("longtext", 0), typeWiden},
		{"postgres varchar(255) to text", pg, str("character varying", 255), str("text", 0), typeWiden},
		{"postgres text to varchar(255)", pg, str("text", 0), str("character varying", 255), typeNarrow},
	}
	for _, tc := range cases {
		if got := typeDirection(tc.driver, tc.from, tc.to); got != tc.want {
			t.Errorf("%s: typeDirection = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// mysqlTablesPlan diffs two MySQL states given as inspected HCL table bodies.
func mysqlTablesPlan(t *testing.T, from, to string) map[string]PlanStep {
	t.Helper()
	build := func(tables string) *schema.Realm {
		hcl := tables + "schema \"dev\" {\n  charset = \"utf8mb4\"\n  collate = \"utf8mb4_0900_ai_ci\"\n}\n"
		r := &schema.Realm{}
		if err := evalHCL(config.DatabaseDriverMySQL, []byte(hcl), r); err != nil {
			t.Fatalf("eval: %v\n%s", err, hcl)
		}
		return r
	}
	changes, err := differ(config.DatabaseDriverMySQL).RealmDiff(build(from), build(to))
	if err != nil {
		t.Fatal(err)
	}
	steps := classifyChanges(config.DatabaseDriverMySQL, changes)
	assertNoDuplicateIDs(t, steps)
	return stepsByID(steps)
}

func TestMySQLNarrowingAndConversionFailures(t *testing.T) {
	skuTable := func(sku string, index string) string {
		return "table \"products\" {\n  schema = schema.dev\n  column \"id\" {\n    null = false\n    type = bigint\n  }\n  column \"sku\" {\n    null = false\n" + sku + "  }\n  primary_key {\n    columns = [column.id]\n  }\n" + index + "}\n"
	}
	unique := "  index \"idx_sku\" {\n    unique  = true\n    columns = [column.sku]\n  }\n"
	t.Run("varchar(255) to tinytext narrows", func(t *testing.T) {
		steps := mysqlTablesPlan(t, skuTable("    type = varchar(255)\n", ""), skuTable("    type = tinytext\n", ""))
		if s := steps["narrow_type:products.sku"]; s.Severity != SeverityDestructive {
			t.Fatalf("steps = %v, want narrow_type:products.sku destructive", steps)
		}
	})
	t.Run("widening past the key limit of a unique key", func(t *testing.T) {
		steps := mysqlTablesPlan(t, skuTable("    type = varchar(255)\n", unique), skuTable("    type = varchar(1000)\n", unique))
		if s := steps["widen_type:products.sku"]; s.Severity != SeverityUnsafe {
			t.Fatalf("widen_type = %+v, want unsafe: 1000 chars x 4 bytes exceeds the 3072-byte key", s)
		}
	})
	t.Run("widening an unindexed column", func(t *testing.T) {
		steps := mysqlTablesPlan(t, skuTable("    type = varchar(255)\n", ""), skuTable("    type = varchar(1000)\n", ""))
		if s := steps["widen_type:products.sku"]; s.Severity != SeverityReview {
			t.Fatalf("widen_type = %+v, want review", s)
		}
	})
	t.Run("utf8mb4 over a long unique key", func(t *testing.T) {
		from := skuTable("    type = varchar(1000)\n    charset = \"latin1\"\n    collate = \"latin1_bin\"\n", unique)
		to := skuTable("    type = varchar(1000)\n    charset = \"utf8mb4\"\n    collate = \"utf8mb4_bin\"\n", unique)
		if s := mysqlTablesPlan(t, from, to)["change_charset:products.sku"]; s.Severity != SeverityUnsafe {
			t.Fatalf("change_charset = %+v, want unsafe: 1000 chars x 4 bytes exceeds the 3072-byte key", s)
		}
	})
	t.Run("utf8mb4 over a short unique key", func(t *testing.T) {
		from := skuTable("    type = varchar(64)\n    charset = \"latin1\"\n    collate = \"latin1_bin\"\n", unique)
		to := skuTable("    type = varchar(64)\n    charset = \"utf8mb4\"\n    collate = \"utf8mb4_bin\"\n", unique)
		if s := mysqlTablesPlan(t, from, to)["change_charset:products.sku"]; s.Severity != SeverityReview {
			t.Fatalf("change_charset = %+v, want review: 256 bytes fits the key", s)
		}
	})

	parent := func(collate string) string {
		return "table \"products\" {\n  schema = schema.dev\n  column \"sku\" {\n    null = false\n    type = varchar(64)\n    collate = \"" + collate + "\"\n  }\n  primary_key {\n    columns = [column.sku]\n  }\n}\n"
	}
	child := func(collate string) string {
		return "table \"order_lines\" {\n  schema = schema.dev\n  column \"id\" {\n    null = false\n    type = bigint\n  }\n  column \"sku\" {\n    null = false\n    type = varchar(64)\n    collate = \"" + collate + "\"\n  }\n  primary_key {\n    columns = [column.id]\n  }\n  foreign_key \"fk_sku\" {\n    columns     = [column.sku]\n    ref_columns = [table.products.column.sku]\n    on_update   = NO_ACTION\n    on_delete   = RESTRICT\n  }\n  index \"fk_sku\" {\n    columns = [column.sku]\n  }\n}\n"
	}
	t.Run("collation on a foreign key child only", func(t *testing.T) {
		steps := mysqlTablesPlan(t, parent("utf8mb4_bin")+child("utf8mb4_bin"), parent("utf8mb4_bin")+child("utf8mb4_0900_ai_ci"))
		if s := steps["change_collation:order_lines.sku"]; s.Severity != SeverityUnsafe {
			t.Fatalf("child collation change = %+v, want unsafe: the parent keeps utf8mb4_bin", s)
		}
	})
	t.Run("collation on a foreign key parent only", func(t *testing.T) {
		steps := mysqlTablesPlan(t, parent("utf8mb4_bin")+child("utf8mb4_bin"), parent("utf8mb4_0900_ai_ci")+child("utf8mb4_bin"))
		if s := steps["change_collation:products.sku"]; s.Severity != SeverityUnsafe {
			t.Fatalf("parent collation change = %+v, want unsafe: the child keeps utf8mb4_bin", s)
		}
	})
	t.Run("collation on both sides of a foreign key", func(t *testing.T) {
		steps := mysqlTablesPlan(t, parent("utf8mb4_bin")+child("utf8mb4_bin"), parent("utf8mb4_0900_ai_ci")+child("utf8mb4_0900_ai_ci"))
		if s := steps["change_collation:order_lines.sku"]; s.Severity != SeverityReview {
			t.Fatalf("child collation change = %+v, want review: both sides move together", s)
		}
		// The parent column is its table's primary key, so its own step is
		// unsafe for the unique-key reason, not the foreign key.
		if s := steps["change_collation:products.sku"]; s.Severity != SeverityUnsafe || strings.Contains(s.Detail, "foreign key") {
			t.Fatalf("parent collation change = %+v, want unsafe for the primary key only", s)
		}
	})
}

// TestMySQLKeyLimitOnEveryBuiltKey: InnoDB refuses a key past 3072 bytes even
// on an empty table, so every step that builds one is unsafe when it can.
func TestMySQLKeyLimitOnEveryBuiltKey(t *testing.T) {
	col := func(name, typ string, null bool) string {
		return fmt.Sprintf("  column %q {\n    null = %v\n    type = %s\n  }\n", name, null, typ)
	}
	table := func(name, body string) string {
		return fmt.Sprintf("table %q {\n  schema = schema.dev\n", name) + col("id", "bigint", false) + body + "  primary_key {\n    columns = [column.id]\n  }\n}\n"
	}
	index := func(name string, unique bool, cols ...string) string {
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = "column." + c
		}
		return fmt.Sprintf("  index %q {\n    unique  = %v\n    columns = [%s]\n  }\n", name, unique, strings.Join(parts, ", "))
	}
	quad := col("a", "varchar(255)", false) + col("b", "varchar(255)", false) + col("c", "varchar(255)", false) + col("d", "varchar(255)", false)

	cases := []struct {
		name     string
		from, to string
		id       string
		want     Severity
	}{
		{
			name: "unique index over a new null-filled varchar(1000)",
			from: table("products", ""),
			to:   table("products", col("sku", "varchar(1000)", true)+index("idx_sku", true, "sku")),
			id:   "add_unique:products.idx_sku", want: SeverityUnsafe,
		},
		{
			name: "unique index over a new null-filled varchar(64)",
			from: table("products", ""),
			to:   table("products", col("sku", "varchar(64)", true)+index("idx_sku", true, "sku")),
			id:   "add_unique:products.idx_sku", want: SeveritySafe,
		},
		{
			name: "non-unique index over four varchar(255)",
			from: table("products", quad),
			to:   table("products", quad+index("idx_abcd", false, "a", "b", "c", "d")),
			id:   "add_index:products.idx_abcd", want: SeverityUnsafe,
		},
		{
			name: "new table with a key past the limit",
			from: "",
			to:   table("products", col("sku", "varchar(1000)", false)+index("idx_sku", true, "sku")),
			id:   "add_table:products", want: SeverityUnsafe,
		},
		{
			name: "widen next to a varbinary in the same key",
			from: table("tokens", col("name", "varchar(100)", false)+col("token", "varbinary(2500)", false)+index("idx_token", true, "name", "token")),
			to:   table("tokens", col("name", "varchar(200)", false)+col("token", "varbinary(2500)", false)+index("idx_token", true, "name", "token")),
			id:   "widen_type:tokens.name", want: SeverityUnsafe,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := mysqlTablesPlan(t, tc.from, tc.to)
			got, ok := steps[tc.id]
			if !ok || got.Severity != tc.want {
				t.Fatalf("step %s = %+v (present %v), want %s; steps %v", tc.id, got, ok, tc.want, steps)
			}
		})
	}
}

func TestKeyBytesIsAnUpperBound(t *testing.T) {
	size := func(n int) *int { return &n }
	c := func(typ schema.Type) *schema.Column {
		return &schema.Column{Name: "c", Type: &schema.ColumnType{Type: typ}}
	}
	for _, tc := range []struct {
		name  string
		parts []*schema.IndexPart
		bytes int
		ok    bool
	}{
		{"varbinary counts its bytes", []*schema.IndexPart{{C: c(&schema.BinaryType{T: "varbinary", Size: size(2500)})}}, 2500, true},
		{"varchar counts 4 bytes a character", []*schema.IndexPart{{C: c(&schema.StringType{T: "varchar", Size: 100})}}, 400, true},
		{"prefix wins", []*schema.IndexPart{{C: c(&schema.StringType{T: "text"}), Attrs: []schema.Attr{&mysql.SubPart{Len: 10}}}}, 40, true},
		{"blob with no prefix", []*schema.IndexPart{{C: c(&schema.BinaryType{T: "blob"})}}, 0, false},
		{"text with no prefix", []*schema.IndexPart{{C: c(&schema.StringType{T: "text"})}}, 0, false},
		{"json", []*schema.IndexPart{{C: c(&schema.JSONType{T: "json"})}}, 0, false},
		{"expression", []*schema.IndexPart{{X: &schema.RawExpr{X: "lower(name)"}}}, 0, false},
	} {
		n, ok := keyBytes(tc.parts)
		if n != tc.bytes || ok != tc.ok {
			t.Errorf("%s: keyBytes = %d, %v; want %d, %v", tc.name, n, ok, tc.bytes, tc.ok)
		}
	}
}

// TestRenamedConstraintsAreSafe: GORM derives index and foreign key names from
// the table, so after a table rename the keys come back under new names with
// the same definition. The rows already satisfy them.
func TestRenamedConstraintsAreSafe(t *testing.T) {
	for _, d := range []struct {
		driver config.DatabaseDriver
		schema string
		idType string
	}{
		{config.DatabaseDriverSQLite, "main", "integer"},
		{config.DatabaseDriverPostgres, "public", "bigint"},
		{config.DatabaseDriverMySQL, "dev", "bigint"},
	} {
		t.Run(string(d.driver), func(t *testing.T) {
			build := func(prefix string) *schema.Realm {
				hcl := fmt.Sprintf(`table "owners" {
  schema = schema.%[1]s
  column "id" {
    null = false
    type = %[2]s
  }
  primary_key {
    columns = [column.id]
  }
}
table "items" {
  schema = schema.%[1]s
  column "id" {
    null = false
    type = %[2]s
  }
  column "sku" {
    null = false
    type = varchar(64)
  }
  column "owner_id" {
    null = true
    type = %[2]s
  }
  primary_key {
    columns = [column.id]
  }
  foreign_key "fk_%[3]s_owner" {
    columns     = [column.owner_id]
    ref_columns = [table.owners.column.id]
    on_update   = NO_ACTION
    on_delete   = RESTRICT
  }
  index "idx_%[3]s_sku" {
    unique  = true
    columns = [column.sku]
  }
  index "idx_%[3]s_owner_id" {
    columns = [column.owner_id]
  }
}
schema %[1]q {}
`, d.schema, d.idType, prefix)
				r := &schema.Realm{}
				if err := evalHCL(d.driver, []byte(hcl), r); err != nil {
					t.Fatalf("eval: %v", err)
				}
				return r
			}
			changes, err := differ(d.driver).RealmDiff(build("products"), build("items"))
			if err != nil {
				t.Fatal(err)
			}
			steps := classifyChanges(d.driver, changes)
			want := map[string]Severity{
				"rename_index:items.idx_items_sku":        SeveritySafe,
				"rename_index:items.idx_items_owner_id":   SeveritySafe,
				"rename_foreign_key:items.fk_items_owner": SeveritySafe,
			}
			switch d.driver {
			case config.DatabaseDriverSQLite:
				// SQLite's differ matches foreign keys by definition, not name.
				delete(want, "rename_foreign_key:items.fk_items_owner")
			case config.DatabaseDriverMySQL:
				// MySQL keeps the index that backs a foreign key, so the new
				// name arrives as a plain (safe) index.
				delete(want, "rename_index:items.idx_items_owner_id")
				want["add_index:items.idx_items_owner_id"] = SeveritySafe
			}
			assertSteps(t, steps, want)
		})
	}
}

func TestDroppedTableLooksRenamed(t *testing.T) {
	in := migrations.Inspection{
		Driver: config.DatabaseDriverSQLite,
		Current: []byte(`table "products" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "name" {
    null = false
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {}
`),
		Desired: []byte(`table "items" {
  schema = schema.main
  column "id" {
    null = false
    type = integer
  }
  column "name" {
    null = false
    type = text
  }
  primary_key {
    columns = [column.id]
  }
}
schema "main" {}
`),
		NewModels:    []migrations.Model{{ImportPath: "example.com/app/internal/item", TypeName: "Item"}},
		ForgetModels: []migrations.Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}},
	}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	drop := stepsByID(plan.Steps)["drop_table:products"]
	want := "gombit db makemigrations <name> --rename-table products:items --model example.com/app/internal/item.Item --forget-model example.com/app/internal/product.Product"
	if !strings.Contains(drop.Hint, want) {
		t.Fatalf("drop_table hint = %q, want %q", drop.Hint, want)
	}
	// Forgetting Product would acknowledge the products drop, but items looks
	// like its rename, so dropping the rows still needs an explicit --allow.
	plan.Acknowledge(nil)
	if pending := plan.Unacknowledged(); len(pending) != 1 || pending[0].ID != "drop_table:products" {
		t.Fatalf("Unacknowledged() = %v, want the likely-renamed drop", stepIDs(pending))
	}
}

// TestTableRenameWorkflowAtlasCLISQLiteWhenAvailable runs the supported
// model-rename workflow end to end with the real loader and Atlas: Product
// (table products) becomes Item (table items), and a stored row survives.
func TestTableRenameWorkflowAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite table-rename test")
		}
	}
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	workDir := t.TempDir()
	migrationDir := filepath.Join(workDir, "database", "migrations")
	product := migrations.Model{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Product"}
	item := migrations.Model{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Item"}
	base := migrations.Options{WorkDir: root, Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Stdout: io.Discard, Stderr: io.Discard}

	// 1. The app starts with Product and a row in products.
	opts := base
	opts.Name, opts.Models = "create_products", []migrations.Model{product}
	if err := migrations.MakeMigrations(ctx, opts); err != nil {
		t.Fatalf("MakeMigrations(create_products) error = %v", err)
	}
	dsn := "file:" + filepath.Join(workDir, "app.db") + "?cache=shared&_fk=1"
	apply := migrations.ApplyOptions{WorkDir: workDir, MigrationDir: migrationDir, AtlasBinary: atlasBin, Database: config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: dsn}, Stdout: io.Discard, Stderr: io.Discard}
	if err := migrations.Migrate(ctx, apply); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	db, err := database.Open(apply.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Exec("INSERT INTO products (name, price) VALUES ('kept', 7)").Error; err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 2. Renamed to Item, a plain diff would drop products. --forget-model
	// does not acknowledge that drop, and the plan names the rename.
	in, err := migrations.Inspect(ctx, migrations.InspectOptions{WorkDir: root, Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Models: []migrations.Model{item}, ForgetModels: []migrations.Model{product}, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	plan, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	plan.Acknowledge(nil)
	drop := stepsByID(plan.Steps)["drop_table:products"]
	if drop.Acknowledged || !strings.Contains(drop.Hint, "--rename-table products:items") {
		t.Fatalf("drop_table:products = %+v, want an unacknowledged drop with the --rename-table hint", drop)
	}

	// 3. The supported path: rename the table and swap the model.
	opts = base
	opts.Name, opts.TableRenames = "rename_products", []migrations.TableRename{{Old: "products", New: "items"}}
	opts.Models, opts.ForgetModels = []migrations.Model{item}, []migrations.Model{product}
	if err := migrations.MakeMigrations(ctx, opts); err != nil {
		t.Fatalf("MakeMigrations(--rename-table) error = %v", err)
	}
	registered, err := migrations.LoadRegistry(migrationDir)
	if err != nil || len(registered) != 1 || registered[0] != item {
		t.Fatalf("registry = %v (%v), want only Item", registered, err)
	}

	// 4. GORM names Item's index after items; the follow-up migration renames
	// it, which the gate lets through without --allow.
	opts = base
	opts.Name, opts.Gate = "sync_items", Gate(nil, io.Discard)
	if err := migrations.MakeMigrations(ctx, opts); err != nil {
		t.Fatalf("MakeMigrations(sync_items) error = %v, want the index rename to pass the gate", err)
	}
	in, err = migrations.Inspect(ctx, migrations.InspectOptions{WorkDir: root, Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if plan, err = Build(in); err != nil || len(plan.Steps) != 0 {
		t.Fatalf("plan after the rename workflow = %v (%v), want no changes", stepIDs(plan.Steps), err)
	}

	// 5. The migrations apply, and the row survives under the new table.
	if err := migrations.Migrate(ctx, apply); err != nil {
		t.Fatalf("Migrate() after rename error = %v", err)
	}
	var name string
	var price int64
	if err := db.Raw("SELECT name, price FROM items").Row().Scan(&name, &price); err != nil || name != "kept" || price != 7 {
		t.Fatalf("items row = %q, %d (%v), want the products row kept", name, price, err)
	}
}

// TestTableRenameSyncOnPostgresAndMySQL classifies real `atlas schema inspect`
// output taken after `ALTER TABLE products RENAME TO items` on PostgreSQL 15
// and MySQL 8, against the schema GORM declares for the renamed model. Only
// GORM's index names differ; the sequence and primary key do not surface.
func TestTableRenameSyncOnPostgresAndMySQL(t *testing.T) {
	for _, d := range []config.DatabaseDriver{config.DatabaseDriverPostgres, config.DatabaseDriverMySQL} {
		t.Run(string(d), func(t *testing.T) {
			in := fixtureInspection(t, d, filepath.Join("rename", string(d)))
			plan, err := Build(in)
			if err != nil {
				t.Fatal(err)
			}
			assertSteps(t, plan.Steps, map[string]Severity{
				"rename_index:items.idx_items_created_at": SeveritySafe,
				"rename_index:items.idx_items_sku":        SeveritySafe,
			})
		})
	}
}

// TestStricterIndexUnderANewNameIsNotARename: a drop and an add pair as a
// rename only when the definition is identical. A dropped predicate, NULLS NOT
// DISTINCT, or a longer prefix can reject rows the old index accepted.
func TestStricterIndexUnderANewNameIsNotARename(t *testing.T) {
	cases := []struct {
		name     string
		driver   config.DatabaseDriver
		schema   string
		strType  string
		from, to string
	}{
		{
			name: "postgres predicate dropped", driver: config.DatabaseDriverPostgres, schema: "public", strType: "text",
			from: "  index \"idx_users_email_active\" {\n    unique  = true\n    columns = [column.email]\n    where   = \"(deleted_at IS NULL)\"\n  }\n",
			to:   "  index \"idx_users_email\" {\n    unique  = true\n    columns = [column.email]\n  }\n",
		},
		{
			name: "sqlite predicate dropped", driver: config.DatabaseDriverSQLite, schema: "main", strType: "text",
			from: "  index \"idx_users_email_active\" {\n    unique  = true\n    columns = [column.email]\n    where   = \"deleted_at IS NULL\"\n  }\n",
			to:   "  index \"idx_users_email\" {\n    unique  = true\n    columns = [column.email]\n  }\n",
		},
		{
			name: "postgres nulls not distinct", driver: config.DatabaseDriverPostgres, schema: "public", strType: "text",
			from: "  index \"idx_users_email_a\" {\n    unique  = true\n    columns = [column.email]\n  }\n",
			to:   "  index \"idx_users_email\" {\n    unique         = true\n    columns        = [column.email]\n    nulls_distinct = false\n  }\n",
		},
		{
			name: "mysql prefix to the full column", driver: config.DatabaseDriverMySQL, schema: "dev", strType: "varchar(255)",
			from: "  index \"idx_users_email_a\" {\n    unique = true\n    on {\n      column = column.email\n      prefix = 20\n    }\n  }\n",
			to:   "  index \"idx_users_email\" {\n    unique  = true\n    columns = [column.email]\n  }\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := func(index string) *schema.Realm {
				ts := "timestamp"
				if tc.driver == config.DatabaseDriverSQLite {
					ts = "datetime"
				}
				hcl := fmt.Sprintf("table \"users\" {\n  schema = schema.%s\n  column \"id\" {\n    null = false\n    type = bigint\n  }\n  column \"email\" {\n    null = true\n    type = %s\n  }\n  column \"deleted_at\" {\n    null = true\n    type = %s\n  }\n  primary_key {\n    columns = [column.id]\n  }\n%s}\nschema %q {}\n", tc.schema, tc.strType, ts, index, tc.schema)
				r := &schema.Realm{}
				if err := evalHCL(tc.driver, []byte(hcl), r); err != nil {
					t.Fatalf("eval: %v\n%s", err, hcl)
				}
				return r
			}
			changes, err := differ(tc.driver).RealmDiff(build(tc.from), build(tc.to))
			if err != nil {
				t.Fatal(err)
			}
			steps := stepsByID(classifyChanges(tc.driver, changes))
			if s, ok := steps["add_unique:users.idx_users_email"]; !ok || s.Severity != SeverityUnsafe {
				t.Fatalf("steps = %v, want add_unique:users.idx_users_email unsafe (no rename)", steps)
			}
			for id := range steps {
				if strings.HasPrefix(id, "rename_") {
					t.Fatalf("stricter index reported as %s", id)
				}
			}
		})
	}
}

func TestCheckRenameRequiresSameAttributes(t *testing.T) {
	tbl := schema.NewTable("items")
	type enforced struct{ schema.Attr }
	drop := &schema.DropCheck{C: &schema.Check{Name: "chk_old", Expr: "(qty > 0)", Attrs: []schema.Attr{&enforced{}}}}
	add := &schema.AddCheck{C: &schema.Check{Name: "chk_new", Expr: "(qty > 0)"}}
	steps := stepsByID(classifyTable(config.DatabaseDriverMySQL, &schema.ModifyTable{T: tbl, Changes: []schema.Change{drop, add}}))
	if s := steps["add_check:items.chk_new"]; s.Severity != SeverityUnsafe {
		t.Fatalf("steps = %v, want add_check unsafe when the attributes differ", steps)
	}
	same := &schema.DropCheck{C: &schema.Check{Name: "chk_old", Expr: "(qty > 0)"}}
	steps = stepsByID(classifyTable(config.DatabaseDriverMySQL, &schema.ModifyTable{T: tbl, Changes: []schema.Change{same, add}}))
	if s := steps["rename_check:items.chk_new"]; s.Severity != SeveritySafe {
		t.Fatalf("steps = %v, want rename_check safe for an identical check", steps)
	}
}

// TestDroppedTableRenameHeuristic: the exact --rename-table command is offered
// only for a single table that holds every non-bookkeeping column with the
// same type, and --forget-model acknowledges a drop only when the plan
// creates no table at all.
func TestDroppedTableRenameHeuristic(t *testing.T) {
	table := func(name string, cols ...string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "table %q {\n  schema = schema.main\n  column \"id\" {\n    null = false\n    type = integer\n  }\n", name)
		for _, c := range cols {
			fmt.Fprintf(&b, "  column %q {\n    null = false\n    type = text\n  }\n", c)
		}
		b.WriteString("  primary_key {\n    columns = [column.id]\n  }\n}\n")
		return b.String()
	}
	forget := []migrations.Model{{ImportPath: "example.com/app/internal/product", TypeName: "Product"}}
	cases := []struct {
		name      string
		current   string
		desired   string
		exact     string // the exact --rename-table target, or ""
		generic   bool   // the hint lists the created tables
		forgetAck bool
	}{
		{"one shared column is not a rename", table("products", "name", "price"), table("coupons", "name", "code"), "", true, false},
		{"model and column renamed together", table("products", "name"), table("items", "title"), "", true, false},
		{"two tables match", table("products", "name"), table("items", "name") + table("goods", "name"), "", true, false},
		{"bookkeeping columns only", table("products"), table("items"), "", true, false},
		{"exact match", table("products", "name", "price"), table("items", "name", "price", "stock"), "items", false, false},
		{"pure retirement", table("products", "name"), "", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Build(migrations.Inspection{
				Driver:       config.DatabaseDriverSQLite,
				Current:      []byte(tc.current + "schema \"main\" {}\n"),
				Desired:      []byte(tc.desired + "schema \"main\" {}\n"),
				ForgetModels: forget,
			})
			if err != nil {
				t.Fatal(err)
			}
			drop := stepsByID(plan.Steps)["drop_table:products"]
			exactCmd := "--rename-table products:" + tc.exact
			if tc.exact != "" && !strings.Contains(drop.Hint, exactCmd+" --forget-model") {
				t.Errorf("hint = %q, want %q", drop.Hint, exactCmd)
			}
			if tc.exact == "" && strings.Contains(drop.Hint, "with the same columns") {
				t.Errorf("hint = %q, want no exact rename suggestion", drop.Hint)
			}
			if tc.generic != strings.Contains(drop.Hint, "--rename-table products:<new_table>") {
				t.Errorf("hint = %q, generic suggestion want %v", drop.Hint, tc.generic)
			}
			plan.Acknowledge(nil)
			if got := stepsByID(plan.Steps)["drop_table:products"].Acknowledged; got != tc.forgetAck {
				t.Errorf("--forget-model acknowledged the drop = %v, want %v", got, tc.forgetAck)
			}
		})
	}
}
