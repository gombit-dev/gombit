package schemaplan

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ariga.io/atlas/sql/schema"

	"github.com/gombit-dev/gombit/config"
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
				"add_column:items.slug":    SeveritySafe,
				"add_index:items.idx_slug": SeveritySafe,
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
		{"mediumtext to text", &schema.StringType{T: "mediumtext"}, &schema.StringType{T: "text"}, typeNarrow},
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

	pg := stepsByID(classifyModifyColumn(config.DatabaseDriverPostgres, "items", change))
	if s := pg["change_type:items.code"]; s.Severity != SeverityUnsafe {
		t.Errorf("postgres text -> bigint = %+v, want an unsafe change_type", s)
	}
	lite := stepsByID(classifyModifyColumn(config.DatabaseDriverSQLite, "items", change))
	if s := lite["change_type:items.code"]; s.Severity != SeverityReview {
		t.Errorf("sqlite text -> bigint = %+v, want a review change_type (affinity keeps values)", s)
	}
	shrink := &schema.ModifyColumn{From: col(&schema.StringType{T: "varchar", Size: 255}, true), To: col(&schema.StringType{T: "varchar", Size: 10}, true), Change: schema.ChangeType}
	if s := stepsByID(classifyModifyColumn(config.DatabaseDriverSQLite, "items", shrink))["change_type:items.code"]; s.Severity != SeverityReview {
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
		&schema.DropForeignKey{F: schema.NewForeignKey("fk_old").AddColumns(child.Columns[0]).SetRefTable(parent).AddRefColumns(parent.Columns[0])},
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
	var stderr bytes.Buffer
	err := Gate([]string{"drop_colum:products.name"}, &stderr)(context.Background(), "reshape_products", in)
	if err == nil || !strings.Contains(err.Error(), "need acknowledgement") || !strings.Contains(err.Error(), "gombit db makemigrations reshape_products --allow") {
		t.Fatalf("Gate() error = %v, want the refusal naming the makemigrations command", err)
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
	if err := Gate(allow, io.Discard)(context.Background(), "reshape", in); err == nil || !strings.Contains(err.Error(), "drop_table:legacy") {
		t.Fatalf("Gate() error = %v, want drop_table:legacy still pending", err)
	}
	if err := Gate(append(allow, "drop_table:legacy"), io.Discard)(context.Background(), "reshape", in); err != nil {
		t.Fatalf("Gate() error = %v, want nil once every step is allowed", err)
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
