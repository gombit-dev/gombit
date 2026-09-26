package schemaplan

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations"
)

func TestDeclaredRenames(t *testing.T) {
	sql := "-- Rename table \"products\" to \"items\"\n" +
		"ALTER TABLE `products` RENAME TO `items`;\n" +
		"ALTER TABLE \"public\".\"items\" RENAME COLUMN \"name\" TO \"title\";\n" +
		"RENAME TABLE orders TO purchases;\n" +
		"-- ALTER TABLE ignored RENAME TO nope;\n" +
		"ALTER TABLE items ADD COLUMN stock integer;\n"
	got := DeclaredRenames(sql)
	want := []DeclaredRename{
		{Table: "products", To: "items"},
		{Table: "items", Column: "name", To: "title"},
		{Table: "orders", To: "purchases"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DeclaredRenames() = %+v, want %+v", got, want)
	}
}

func TestBuildWithRenamesKeepsOtherChanges(t *testing.T) {
	current := []byte(`table "products" {
  schema = schema.public
  column "id" {
    null = false
    type = bigint
  }
  column "name" {
    null = false
    type = character_varying(255)
  }
  primary_key {
    columns = [column.id]
  }
}
schema "public" {}
`)
	desired := []byte(`table "items" {
  schema = schema.public
  column "id" {
    null = false
    type = bigint
  }
  column "title" {
    null = false
    type = character_varying(100)
  }
  primary_key {
    columns = [column.id]
  }
}
schema "public" {}
`)
	in := migrations.Inspection{Driver: config.DatabaseDriverPostgres, Current: current, Desired: desired}
	plan, err := BuildWithRenames(in, []DeclaredRename{{Table: "products", To: "items"}, {Table: "items", Column: "name", To: "title"}})
	if err != nil {
		t.Fatal(err)
	}
	// The renames are safe, but the narrowed type on the renamed column is
	// still destructive: pairing by the declared rename must not hide it.
	assertSteps(t, plan.Steps, map[string]Severity{
		"rename_table:items":        SeveritySafe,
		"rename_column:items.title": SeveritySafe,
		"narrow_type:items.title":   SeverityDestructive,
	})
	plain, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stepsByID(plain.Steps)["drop_table:products"]; !ok {
		t.Fatalf("without the declared renames the diff is a drop: %v", stepIDs(plain.Steps))
	}
}

// TestLintAndRepairAtlasCLISQLiteWhenAvailable runs the lint/repair workflow
// with the real loader and Atlas.
func TestLintAndRepairAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		atlasBin, err = exec.LookPath("atlas")
		if err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite lint test")
		}
	}
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(wd))
	migrationDir := t.TempDir()
	product := migrations.Model{ImportPath: "github.com/gombit-dev/gombit/migrations/testmodels", TypeName: "Product"}
	lint := func(latest int) LintReport {
		t.Helper()
		r, err := Lint(ctx, LintOptions{WorkDir: root, Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Latest: latest, Stderr: io.Discard})
		if err != nil {
			t.Fatalf("Lint() error = %v", err)
		}
		return r
	}
	write := func(name, sql string) string {
		t.Helper()
		files, _ := migrations.ListMigrationFiles(migrationDir)
		version := "20260101000000"
		if n := len(files); n > 0 {
			last, err := strconv.ParseInt(files[n-1].Version, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			version = strconv.FormatInt(last+1, 10)
		}
		path := filepath.Join(migrationDir, version+"_"+name+".sql")
		if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	hash := func() {
		t.Helper()
		if err := migrations.Hash(ctx, migrations.ApplyOptions{WorkDir: root, MigrationDir: migrationDir, AtlasBinary: atlasBin, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
			t.Fatalf("Hash() error = %v", err)
		}
	}

	// 1. A hand-written first migration with an extra column, then the model
	// drops it through makemigrations --allow: the directive is written, so
	// the migration lints clean.
	write("create_products", "CREATE TABLE `products` (`id` integer NULL PRIMARY KEY AUTOINCREMENT, `created_at` datetime NULL, `updated_at` datetime NULL, `deleted_at` datetime NULL, `name` varchar(120) NOT NULL, `price` integer NOT NULL, `legacy` text NULL);\nCREATE INDEX `idx_products_deleted_at` ON `products` (`deleted_at`);\n")
	if err := migrations.SaveRegistry(migrationDir, []migrations.Model{product}); err != nil {
		t.Fatal(err)
	}
	hash()
	err = migrations.MakeMigrations(ctx, migrations.Options{WorkDir: root, Name: "drop_legacy", Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin, Gate: Gate([]string{"drop_column:products.legacy"}, io.Discard), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("MakeMigrations(--allow) error = %v", err)
	}
	files, _ := migrations.ListMigrationFiles(migrationDir)
	generated, _ := os.ReadFile(files[len(files)-1].UpPath)
	if !slices.Contains(migrations.AllowDirectives(string(generated)), "drop_column:products.legacy") {
		t.Fatalf("generated migration has no directive:\n%s", generated)
	}
	// The drop is an Atlas SQLite rebuild (copy into new_products, DROP
	// TABLE products, rename back): the statement-level DROP TABLE is the
	// copy's second half, not data loss, so the directive for the column drop
	// is all it needs.
	if r := lint(0); r.Failed() {
		t.Fatalf("lint of the acknowledged migration failed: %+v", r)
	}

	// 2. A hand edit breaks the checksum; lint names the gombit recovery.
	f, _ := os.OpenFile(files[len(files)-1].UpPath, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("-- reviewed\n")
	_ = f.Close()
	r := lint(0)
	if !strings.Contains(r.Integrity, "gombit db repair") || strings.Contains(r.Integrity, "atlas migrate") {
		t.Fatalf("integrity = %q, want the gombit recovery and no raw Atlas command", r.Integrity)
	}
	err = migrations.ValidateDir(ctx, migrations.DirOptions{WorkDir: root, Driver: config.DatabaseDriverSQLite, MigrationDir: migrationDir, AtlasBinary: atlasBin})
	var cerr *migrations.ChecksumError
	if !errors.As(err, &cerr) || len(cerr.Files) != 1 || !strings.HasSuffix(cerr.Files[0], "_drop_legacy.sql") {
		t.Fatalf("ValidateDir() = %v, want a checksum error naming the edited file", err)
	}
	hash()

	// 3. A hand-written destructive migration without a directive fails lint
	// until it carries one.
	drop := write("drop_price", "ALTER TABLE `products` DROP COLUMN `price`;\n")
	hash()
	r = lint(0)
	pending := r.Unacknowledged()
	if len(pending) != 1 || pending[0].Step.ID != "drop_column:products.price" {
		t.Fatalf("Unacknowledged() = %+v, want the hand-written drop", pending)
	}
	if err := os.WriteFile(drop, []byte("-- gombit:allow drop_column:products.price\nALTER TABLE `products` DROP COLUMN `price`;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash()
	if r := lint(0); r.Failed() {
		t.Fatalf("lint with the directive failed: %+v", r)
	}

	// 4. A declared table rename lints as safe, not as a drop.
	write("rename_products", "ALTER TABLE `products` RENAME TO `items`;\n")
	hash()
	r = lint(0)
	if r.Failed() {
		t.Fatalf("lint of a declared rename failed: %+v", r)
	}
	last := r.Migrations[len(r.Migrations)-1]
	if s := stepsByID(last.Steps)["rename_table:items"]; s.Severity != SeveritySafe {
		t.Fatalf("rename migration steps = %v, want rename_table:items safe", stepIDs(last.Steps))
	}
	if len(r.Migrations) != 4 {
		t.Fatalf("the default classified %d migrations, want all 4", len(r.Migrations))
	}

	// 5. Data loss the before/after schema cannot show still needs a
	// directive: each of these leaves the same shape behind.
	for _, tc := range []struct {
		name, sql, id string
	}{
		{"delete_rows", "DELETE FROM `items`;\n", "data_change:items"},
		{"recreate_items", "DROP TABLE `items`;\nCREATE TABLE `items` (`id` integer NULL PRIMARY KEY AUTOINCREMENT, `created_at` datetime NULL, `updated_at` datetime NULL, `deleted_at` datetime NULL, `name` varchar(120) NOT NULL);\nCREATE INDEX `idx_products_deleted_at` ON `items` (`deleted_at`);\n", "drop_table:items"},
		{"rename_then_recreate", "ALTER TABLE `items` RENAME TO `goods`;\nDROP TABLE `goods`;\nCREATE TABLE `goods` (`id` integer NULL PRIMARY KEY AUTOINCREMENT, `created_at` datetime NULL, `updated_at` datetime NULL, `deleted_at` datetime NULL, `name` varchar(120) NOT NULL);\nCREATE INDEX `idx_products_deleted_at` ON `goods` (`deleted_at`);\n", "drop_table:goods"},
	} {
		path := write(tc.name, tc.sql)
		hash()
		r := lint(1)
		steps := stepsByID(r.Migrations[0].Steps)
		if s, ok := steps[tc.id]; !ok || !s.NeedsAcknowledgement() {
			t.Fatalf("%s: steps = %v, want an unacknowledged %s", tc.name, stepIDs(r.Migrations[0].Steps), tc.id)
		}
		if _, ok := steps["rename_table:goods"]; ok {
			t.Fatalf("%s: a rename whose table is then dropped was reported as safe", tc.name)
		}
		if err := os.WriteFile(path, []byte("-- gombit:allow "+tc.id+"\n"+tc.sql), 0o600); err != nil {
			t.Fatal(err)
		}
		hash()
		if r := lint(1); r.Failed() {
			t.Fatalf("%s with its directive failed: %+v", tc.name, r)
		}
	}
}

// TestLintDefaultCoversEveryMigration: a safe migration after an
// unacknowledged destructive one must not hide it from the default run.
func TestLintDefaultCoversEveryMigrationAtlasCLISQLiteWhenAvailable(t *testing.T) {
	atlasBin := os.Getenv("ATLAS_BINARY")
	if atlasBin == "" {
		var err error
		if atlasBin, err = exec.LookPath("atlas"); err != nil {
			t.Skip("Atlas CLI not found; set ATLAS_BINARY to run the real SQLite lint test")
		}
	}
	dir := t.TempDir()
	for name, sql := range map[string]string{
		"20260101000000_create_products.sql": "CREATE TABLE products (id integer PRIMARY KEY, price integer NOT NULL);\n",
		"20260102000000_drop_price.sql":      "ALTER TABLE products DROP COLUMN price;\n",
		"20260103000000_add_note.sql":        "ALTER TABLE products ADD COLUMN note text;\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := migrations.Hash(ctx, migrations.ApplyOptions{WorkDir: dir, MigrationDir: dir, AtlasBinary: atlasBin, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	run := func(latest int) LintReport {
		r, err := Lint(ctx, LintOptions{WorkDir: dir, Driver: config.DatabaseDriverSQLite, MigrationDir: dir, AtlasBinary: atlasBin, Latest: latest, Stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := run(0)
	if !r.Failed() || len(r.Unacknowledged()) != 1 || r.Unacknowledged()[0].File != "20260102000000_drop_price.sql" {
		t.Fatalf("default lint = %+v, want the older drop reported", r.Unacknowledged())
	}
	if r := run(1); r.Failed() {
		t.Fatalf("--latest 1 classifies only add_note and should pass: %+v", r)
	}
}

// lintSteps classifies one migration the way Lint does, from HCL states and
// its SQL, without Atlas: the diff with declared renames, plus the statement
// findings.
func lintSteps(t *testing.T, driver config.DatabaseDriver, current, desired, sql string) map[string]PlanStep {
	t.Helper()
	plan, after, err := buildWithRenames(migrations.Inspection{Driver: driver, Current: []byte(current), Desired: []byte(desired)}, DeclaredRenames(sql))
	if err != nil {
		t.Fatal(err)
	}
	return stepsByID(withStatementFindings(plan.Steps, sql, after))
}

func pgProducts(col, typ, def string) string {
	d := ""
	if def != "" {
		d = "    default = " + def + "\n"
	}
	return "table \"products\" {\n  schema = schema.public\n  column \"id\" {\n    null = false\n    type = bigint\n  }\n  column \"" + col + "\" {\n    null = true\n    type = " + typ + "\n" + d + "  }\n  primary_key {\n    columns = [column.id]\n  }\n}\nschema \"public\" {}\n"
}

func TestAlterColumnUsingIsNeverHidden(t *testing.T) {
	pg := config.DatabaseDriverPostgres
	cases := []struct {
		name           string
		current, want  string
		sql            string
		alterID        string // "" means no alter_column finding
		mustStayReview string
	}{
		{"USING beside a widen", pgProducts("name", "character_varying(255)", ""), pgProducts("name", "text", ""),
			"ALTER TABLE products ALTER COLUMN name TYPE text USING left(name, 1);", "alter_column:products.name", "widen_type:products.name"},
		{"USING after a declared rename", pgProducts("name", "character_varying(255)", ""), pgProducts("title", "text", ""),
			"ALTER TABLE products RENAME COLUMN name TO title;\nALTER TABLE products ALTER COLUMN title TYPE text USING 'redacted';", "alter_column:products.title", ""},
		{"implicit widen", pgProducts("name", "character_varying(255)", ""), pgProducts("name", "text", ""),
			"ALTER TABLE products ALTER COLUMN name TYPE text;", "", "widen_type:products.name"},
		{"set default", pgProducts("name", "text", ""), pgProducts("name", "text", `"x"`),
			"ALTER TABLE products ALTER COLUMN name SET DEFAULT 'x';", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := lintSteps(t, pg, tc.current, tc.want, tc.sql)
			var ids []string
			for id := range steps {
				ids = append(ids, id)
			}
			if tc.alterID == "" {
				for id := range steps {
					if strings.HasPrefix(id, "alter_column:") {
						t.Fatalf("steps = %v, want no alter_column finding", ids)
					}
				}
			} else if s, ok := steps[tc.alterID]; !ok || !s.NeedsAcknowledgement() {
				t.Fatalf("steps = %v, want an unacknowledged %s", ids, tc.alterID)
			}
			if tc.mustStayReview != "" && steps[tc.mustStayReview].Severity != SeverityReview {
				t.Fatalf("steps = %v, want %s review", ids, tc.mustStayReview)
			}
		})
	}
}

func sqliteItems(cols ...string) string {
	var b strings.Builder
	b.WriteString("table \"items\" {\n  schema = schema.main\n  column \"id\" {\n    null = false\n    type = integer\n  }\n")
	for _, c := range cols {
		b.WriteString("  column \"" + c + "\" {\n    null = false\n    type = text\n  }\n")
	}
	b.WriteString("  primary_key {\n    columns = [column.id]\n  }\n}\nschema \"main\" {}\n")
	return b.String()
}

func TestRebuildExemptionIsExact(t *testing.T) {
	lite := config.DatabaseDriverSQLite
	rebuild := func(cols, sel string) string {
		return "CREATE TABLE `new_items` (`id` integer PRIMARY KEY, `name` text NOT NULL);\n" +
			"INSERT INTO `new_items` (" + cols + ") SELECT " + sel + " FROM `items`;\n" +
			"DROP TABLE `items`;\n" +
			"ALTER TABLE `new_items` RENAME TO `items`;\n"
	}
	cases := []struct {
		name          string
		current, want string
		sql           string
		dropFinding   bool
	}{
		{"a constant is not a copy", sqliteItems("name"), sqliteItems("name"), rebuild("`id`, `name`", "`id`, 'gone'"), true},
		{"an exact copy with nothing rebuilt", sqliteItems("name"), sqliteItems("name"), rebuild("`id`, `name`", "`id`, `name`"), true},
		{"Atlas's rebuild for a dropped column", sqliteItems("name", "legacy"), sqliteItems("name"), rebuild("`id`, `name`", "`id`, `name`"), false},
		{"a second drop after a real rebuild", sqliteItems("name", "legacy"), sqliteItems("name"),
			rebuild("`id`, `name`", "`id`, `name`") + "DROP TABLE `items`;\nCREATE TABLE `items` (`id` integer PRIMARY KEY, `name` text NOT NULL);\n", true},
		{"no rename back", sqliteItems("name", "legacy"), sqliteItems("name"),
			"CREATE TABLE `new_items` (`id` integer PRIMARY KEY, `name` text NOT NULL);\nINSERT INTO `new_items` (`id`, `name`) SELECT `id`, `name` FROM `items`;\nDROP TABLE `items`;\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := lintSteps(t, lite, tc.current, tc.want, tc.sql)
			s, ok := steps["drop_table:items"]
			if tc.dropFinding != (ok && s.NeedsAcknowledgement()) {
				var ids []string
				for id := range steps {
					ids = append(ids, id)
				}
				t.Fatalf("steps = %v, want drop_table:items finding = %v", ids, tc.dropFinding)
			}
		})
	}
}

// TestPartialRebuildCopyIsNotACopy: a copy into new_X that leaves out a column
// X still has afterwards empties that column in every row.
func TestPartialRebuildCopyIsNotACopy(t *testing.T) {
	items := func(def string) string {
		return "table \"items\" {\n  schema = schema.main\n  column \"id\" {\n    null = false\n    type = integer\n  }\n" +
			"  column \"name\" {\n    null    = false\n    type    = text\n    default = \"" + def + "\"\n  }\n" +
			"  column \"code\" {\n    null = true\n    type = text\n  }\n  primary_key {\n    columns = [column.id]\n  }\n}\nschema \"main\" {}\n"
	}
	sql := "CREATE TABLE `new_items` (`id` integer NOT NULL, `name` text NOT NULL DEFAULT 'changed', `code` text, PRIMARY KEY (`id`));\n" +
		"INSERT INTO `new_items` (`id`, `name`) SELECT `id`, `name` FROM `items`;\n" +
		"DROP TABLE `items`;\n" +
		"ALTER TABLE `new_items` RENAME TO `items`;\n"
	steps := lintSteps(t, config.DatabaseDriverSQLite, items("keep"), items("changed"), sql)
	if s, ok := steps["drop_table:items"]; !ok || !s.NeedsAcknowledgement() {
		t.Fatalf("steps = %v, want drop_table:items: the copy leaves code out", keysOf(steps))
	}
	full := strings.Replace(sql, "INSERT INTO `new_items` (`id`, `name`) SELECT `id`, `name` FROM `items`", "INSERT INTO `new_items` (`id`, `name`, `code`) SELECT `id`, `name`, `code` FROM `items`", 1)
	if _, ok := lintSteps(t, config.DatabaseDriverSQLite, items("keep"), items("changed"), full)["drop_table:items"]; ok {
		t.Fatal("a copy of every surviving column is the rebuild and must not count as a drop")
	}
}

// TestUsingBesideDropColumn: every action of an ALTER TABLE is read, so a
// USING rewrite is not hidden by a DROP COLUMN in the same statement, and
// acknowledging the drop does not acknowledge the rewrite.
func TestUsingBesideDropColumn(t *testing.T) {
	current := "table \"products\" {\n  schema = schema.public\n  column \"id\" {\n    null = false\n    type = bigint\n  }\n  column \"name\" {\n    null = true\n    type = character_varying(255)\n  }\n  column \"legacy\" {\n    null = true\n    type = text\n  }\n  primary_key {\n    columns = [column.id]\n  }\n}\nschema \"public\" {}\n"
	desired := pgProducts("name", "text", "")
	for _, sql := range []string{
		"ALTER TABLE products ALTER COLUMN name TYPE text USING left(name, 1), DROP COLUMN legacy;",
		"ALTER TABLE products DROP COLUMN legacy, ALTER COLUMN name TYPE text USING left(name, 1);",
	} {
		steps := lintSteps(t, config.DatabaseDriverPostgres, current, desired, sql)
		if s, ok := steps["alter_column:products.name"]; !ok || s.Severity != SeverityDestructive {
			t.Fatalf("%s: steps = %v, want alter_column:products.name destructive", sql, keysOf(steps))
		}
		for id := range steps {
			if strings.HasSuffix(id, ",") {
				t.Fatalf("%s: step id %q keeps the comma", sql, id)
			}
		}
		plan := SchemaPlan{}
		for _, s := range steps {
			plan.Steps = append(plan.Steps, s)
		}
		plan.Acknowledge([]string{"drop_column:products.legacy"})
		pending := plan.Unacknowledged()
		if len(pending) != 1 || pending[0].ID != "alter_column:products.name" {
			t.Fatalf("%s: after acknowledging the drop, pending = %v; want the rewrite", sql, stepIDs(pending))
		}
	}
}

func TestSplitActions(t *testing.T) {
	got := splitActions("ADD COLUMN c numeric(10,2) DEFAULT 1, ALTER COLUMN d TYPE text USING concat(d, ','), DROP COLUMN e")
	want := []string{"ADD COLUMN c numeric(10,2) DEFAULT 1", "ALTER COLUMN d TYPE text USING concat(d, ',')", "DROP COLUMN e"}
	if !slices.Equal(got, want) {
		t.Fatalf("splitActions() = %q, want %q", got, want)
	}
}

func keysOf(m map[string]PlanStep) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
