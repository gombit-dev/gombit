package upgrade_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/migrations"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/upgrade"
)

// frameworkSchemaSnapshot is the framework-owned schema (the tables of
// upgrade.FrameworkModels) as of a release the manifest lists: what the
// code's models are compared with, through the schema changes the releases
// after it declare.
const frameworkSchemaSnapshot = "testdata/framework-schema.json"

type schemaSnapshot struct {
	Version string        `json:"version"`
	Tables  []schemaTable `json:"tables"`
}

type schemaTable struct {
	Table   string         `json:"table"`
	Model   string         `json:"model"`
	Columns []schemaColumn `json:"columns"`
	Indexes []schemaIndex  `json:"indexes"`
}

type schemaColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	PrimaryKey bool   `json:"primary_key,omitempty"`
	NotNull    bool   `json:"not_null,omitempty"`
	Unique     bool   `json:"unique,omitempty"`
	Default    string `json:"default,omitempty"`
}

type schemaIndex struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique,omitempty"`
	Columns []string `json:"columns"`
}

// frameworkModels are the framework's own Models(), the ones the catalog
// stands for.
func frameworkModels() []any { return append(auth.Models(), claims.Models()...) }

// currentFrameworkSchema is the framework-owned schema the code defines, as
// GORM (and so the Atlas GORM provider) reads the models: each model's
// table, and each many-to-many join table it declares.
func currentFrameworkSchema(t *testing.T) map[string]schemaTable {
	t.Helper()
	cache := &sync.Map{}
	tables := map[string]schemaTable{}
	for _, m := range frameworkModels() {
		s, err := schema.Parse(m, cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		typ := reflect.TypeOf(m).Elem()
		model := strings.TrimPrefix(typ.PkgPath(), "github.com/gombit-dev/gombit/") + "." + typ.Name()
		add := func(s *schema.Schema) {
			st := schemaTable{Table: s.Table, Model: model, Columns: []schemaColumn{}, Indexes: []schemaIndex{}}
			for _, f := range s.Fields {
				if f.DBName == "" {
					continue
				}
				typ := string(f.DataType)
				switch {
				case f.Precision > 0 || f.Scale > 0:
					typ += fmt.Sprintf("(%d,%d)", f.Precision, f.Scale)
				case f.Size > 0:
					typ += fmt.Sprintf("(%d)", f.Size)
				}
				def := ""
				if f.HasDefaultValue {
					def = f.DefaultValue
				}
				st.Columns = append(st.Columns, schemaColumn{Name: f.DBName, Type: typ, PrimaryKey: f.PrimaryKey, NotNull: f.NotNull, Unique: f.Unique, Default: def})
			}
			for _, idx := range s.ParseIndexes() {
				si := schemaIndex{Name: idx.Name, Unique: idx.Class == "UNIQUE", Columns: []string{}}
				for _, o := range idx.Fields {
					si.Columns = append(si.Columns, o.DBName)
				}
				st.Indexes = append(st.Indexes, si)
			}
			slices.SortFunc(st.Indexes, func(a, b schemaIndex) int { return strings.Compare(a.Name, b.Name) })
			tables[st.Table] = st
		}
		add(s)
		for _, rel := range s.Relationships.Many2Many {
			add(rel.JoinTable)
		}
	}
	return tables
}

// TestFrameworkModelsAreTheFrameworksTables: upgrade.FrameworkModels is
// every model the framework's Models() functions return, and those are
// every package of the framework that exports one: a framework package
// that gains tables is in the catalog, so its schema is held to the
// manifest below.
func TestFrameworkModelsAreTheFrameworksTables(t *testing.T) {
	var got []migrations.Model
	for _, m := range frameworkModels() {
		typ := reflect.TypeOf(m).Elem()
		got = append(got, migrations.Model{ImportPath: typ.PkgPath(), TypeName: typ.Name()})
	}
	if !slices.Equal(got, upgrade.FrameworkModels) {
		t.Fatalf("FrameworkModels = %v, want the framework's Models(): %v", upgrade.FrameworkModels, got)
	}
	catalogued := map[string]bool{}
	for _, m := range upgrade.FrameworkModels {
		catalogued[strings.TrimPrefix(m.ImportPath, "github.com/gombit-dev/gombit/")] = true
	}
	skip := map[string]bool{"examples": true, "benchmarks": true, "node_modules": true, "testdata": true, ".git": true, "goldentest": true, "scaffold": true}
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "Models" {
				pkg, _ := filepath.Rel("..", filepath.Dir(path))
				if !catalogued[filepath.ToSlash(pkg)] {
					t.Errorf("%s exports Models(), but upgrade.FrameworkModels does not list its models: add them, so its tables are held to the manifest", path)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFrameworkSchemaIsDeclared: the framework-owned schema the code
// defines is the snapshot's (a release's), as the manifest's later
// releases, unreleased included, change it. A table that is new needs an
// added declaration, one that changes an altered one, one that is gone a
// removed one; and a declaration that describes nothing (added for a table
// that was there, altered for one that did not change, removed for one
// still defined) fails. -update moves the snapshot to the newest release
// when it is cut (unreleased declares nothing left).
func TestFrameworkSchemaIsDeclared(t *testing.T) {
	data, err := os.ReadFile(frameworkSchemaSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var snap schemaSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	current := currentFrameworkSchema(t)
	if *update {
		if r, err := m.Release(upgrade.Unreleased); err == nil && slices.ContainsFunc(r.Changes, func(c upgrade.Change) bool { return len(c.Schema) > 0 }) {
			t.Fatal("unreleased still declares framework-schema changes: -update moves the snapshot to a release, once they are cut")
		}
		next := schemaSnapshot{Version: m.Latest(), Tables: []schemaTable{}}
		for _, st := range current {
			next.Tables = append(next.Tables, st)
		}
		slices.SortFunc(next.Tables, func(a, b schemaTable) int { return strings.Compare(a.Table, b.Table) })
		out, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(frameworkSchemaSnapshot, append(out, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := m.Release(snap.Version); err != nil {
		t.Fatalf("the snapshot's version: %v", err)
	}
	declared := map[string][]upgrade.SchemaChangeKind{}
	after := false
	for _, r := range m.Releases {
		if after {
			for _, c := range r.Changes {
				for _, sc := range c.Schema {
					declared[sc.Table] = append(declared[sc.Table], sc.Change)
				}
			}
		}
		after = after || r.Version == snap.Version
	}
	old := map[string]schemaTable{}
	for _, st := range snap.Tables {
		old[st.Table] = st
	}
	for _, problem := range schemaProblems(old, current, declared) {
		t.Error(problem)
	}
}

// schemaProblems compares the snapshot's tables with the code's, given the
// kinds of change declared for each table since.
func schemaProblems(old, current map[string]schemaTable, declared map[string][]upgrade.SchemaChangeKind) []string {
	var problems []string
	has := func(table string, k upgrade.SchemaChangeKind) bool { return slices.Contains(declared[table], k) }
	declare := func(st schemaTable, k upgrade.SchemaChangeKind) string {
		return fmt.Sprintf("under unreleased in upgrade/manifest.yaml, a manual change in the schema area with\n        schema:\n          - {model: %s, table: %s, change: %s}", st.Model, st.Table, k)
	}
	for _, table := range sortedTables(current) {
		cur := current[table]
		prev, existed := old[table]
		switch {
		case !existed && !has(table, upgrade.SchemaAdded):
			problems = append(problems, fmt.Sprintf("framework table %s (%s) is new since the snapshot; declare it %s", table, cur.Model, declare(cur, upgrade.SchemaAdded)))
		case existed && !reflect.DeepEqual(prev, cur) && !has(table, upgrade.SchemaAltered) && !has(table, upgrade.SchemaAdded):
			problems = append(problems, fmt.Sprintf("framework table %s (%s) changed since the snapshot (%s); declare it %s", table, cur.Model, tableDiff(prev, cur), declare(cur, upgrade.SchemaAltered)))
		case existed && reflect.DeepEqual(prev, cur) && (has(table, upgrade.SchemaAltered) || has(table, upgrade.SchemaAdded)) && !has(table, upgrade.SchemaRemoved):
			problems = append(problems, fmt.Sprintf("framework table %s is declared %v, but it is as the snapshot has it: the declaration describes no change", table, declared[table]))
		}
		if has(table, upgrade.SchemaRemoved) && !has(table, upgrade.SchemaAdded) {
			problems = append(problems, fmt.Sprintf("framework table %s is declared removed, but the framework still defines it (%s)", table, cur.Model))
		}
	}
	for _, table := range sortedTables(old) {
		if _, ok := current[table]; !ok && !has(table, upgrade.SchemaRemoved) {
			problems = append(problems, fmt.Sprintf("framework table %s (%s) is gone since the snapshot; declare it %s", table, old[table].Model, declare(old[table], upgrade.SchemaRemoved)))
		}
	}
	for table := range declared {
		if _, ok := current[table]; !ok && (!has(table, upgrade.SchemaAdded) || !has(table, upgrade.SchemaRemoved)) {
			if _, was := old[table]; !was {
				problems = append(problems, fmt.Sprintf("framework table %s is declared %v, but no framework model defines it, now or in the snapshot", table, declared[table]))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func sortedTables(m map[string]schemaTable) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// tableDiff names what changed in a table: columns added, removed or
// changed, and indexes.
func tableDiff(prev, cur schemaTable) string {
	var parts []string
	cols := func(st schemaTable) map[string]schemaColumn {
		out := map[string]schemaColumn{}
		for _, c := range st.Columns {
			out[c.Name] = c
		}
		return out
	}
	p, c := cols(prev), cols(cur)
	for _, col := range cur.Columns {
		switch old, ok := p[col.Name]; {
		case !ok:
			parts = append(parts, "column "+col.Name+" added")
		case old != col:
			parts = append(parts, "column "+col.Name+" changed")
		}
	}
	for _, col := range prev.Columns {
		if _, ok := c[col.Name]; !ok {
			parts = append(parts, "column "+col.Name+" removed")
		}
	}
	if !reflect.DeepEqual(prev.Indexes, cur.Indexes) {
		parts = append(parts, "indexes changed")
	}
	if prev.Model != cur.Model {
		parts = append(parts, "model "+prev.Model+" -> "+cur.Model)
	}
	return strings.Join(parts, ", ")
}

// TestSchemaProblems: the guard's rules, on fixed tables.
func TestSchemaProblems(t *testing.T) {
	a := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}}}
	a2 := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}, {Name: "x", Type: "string"}}}
	b := schemaTable{Table: "b", Model: "auth.B", Columns: []schemaColumn{}}
	tables := func(ts ...schemaTable) map[string]schemaTable {
		out := map[string]schemaTable{}
		for _, st := range ts {
			out[st.Table] = st
		}
		return out
	}
	kinds := func(pairs ...string) map[string][]upgrade.SchemaChangeKind {
		out := map[string][]upgrade.SchemaChangeKind{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out[pairs[i]] = append(out[pairs[i]], upgrade.SchemaChangeKind(pairs[i+1]))
		}
		return out
	}
	for name, tc := range map[string]struct {
		old, cur map[string]schemaTable
		declared map[string][]upgrade.SchemaChangeKind
		want     string
	}{
		"unchanged":               {tables(a), tables(a), kinds(), ""},
		"new table":               {tables(a), tables(a, b), kinds(), "framework table b (auth.B) is new"},
		"new table, declared":     {tables(a), tables(a, b), kinds("b", "added"), ""},
		"altered":                 {tables(a), tables(a2), kinds(), "column x added"},
		"altered, declared":       {tables(a), tables(a2), kinds("a", "altered"), ""},
		"gone":                    {tables(a, b), tables(a), kinds(), "framework table b (auth.B) is gone"},
		"gone, declared":          {tables(a, b), tables(a), kinds("b", "removed"), ""},
		"altered, but no change":  {tables(a), tables(a), kinds("a", "altered"), "describes no change"},
		"removed, still there":    {tables(a), tables(a), kinds("a", "removed"), "still defines it"},
		"declared, no such table": {tables(a), tables(a), kinds("zz", "added"), "no framework model defines it"},
		"added, then removed":     {tables(a), tables(a), kinds("zz", "added", "zz", "removed"), ""},
	} {
		got := strings.Join(schemaProblems(tc.old, tc.cur, tc.declared), "\n")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", name, got, tc.want)
		}
	}
}

// schemaRelease is a release with one change declaring schema entries.
func schemaRelease(version, kind string, entries ...upgrade.SchemaChange) upgrade.Release {
	return upgrade.Release{Version: version, Changes: []upgrade.Change{{ID: "schema-" + strings.ReplaceAll(version, ".", "-"), Kind: upgrade.Kind(kind), Area: "schema", Summary: "s", Details: "d", Schema: entries}}}
}

// registryApp is an application directory whose model registry lists
// models (none: no registry file).
func registryApp(t *testing.T, models ...migrations.Model) string {
	t.Helper()
	dir := t.TempDir()
	if models != nil {
		migrationDir := filepath.Join(dir, "database", "migrations")
		if err := os.MkdirAll(migrationDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := migrations.SaveRegistry(migrationDir, models); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestAnalyzeSchema: per change, what the application's model registry
// says about it, and the migration steps the upgrade needs: an altered
// table the registry lists, an added model it lacks, a removed one it
// lists need a migration (generated, reviewed and applied by the
// developer); a table the application does not have, or one an
// informational change adds for an optional feature, does not.
func TestAnalyzeSchema(t *testing.T) {
	refresh := upgrade.SchemaChange{Model: "auth.RefreshToken", Table: "refresh_tokens", Change: upgrade.SchemaAltered}
	claim := upgrade.SchemaChange{Model: "storage/claims.Claim", Table: "storage_claims", Change: upgrade.SchemaAdded}
	authRefresh := migrations.Model{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "RefreshToken"}
	claimModel := migrations.Model{ImportPath: "github.com/gombit-dev/gombit/storage/claims", TypeName: "Claim"}

	// Altered and registered: a migration; added and not registered, in a
	// manual change: add the model, then a migration.
	dir := registryApp(t, authRefresh)
	r, err := upgrade.AnalyzeSchema(dir, "", []upgrade.Release{schemaRelease("v0.9.0", "manual", refresh, claim)})
	if err != nil {
		t.Fatal(err)
	}
	if !r.RegistryFound || r.Registry != "database/migrations/models.json" || len(r.Findings) != 2 || !r.Findings[0].Required || !r.Findings[0].Registered || !r.Findings[1].Required || r.Findings[1].Registered || !r.MigrationRequired() {
		t.Fatalf("report = %+v", r)
	}
	steps := strings.Join(r.Steps, "\n")
	for _, want := range []string{
		"AutoMigrate (internal/platform/database.go): github.com/gombit-dev/gombit/storage/claims.Claim",
		"gombit db plan",
		"gombit db makemigrations upgrade_framework_schema --model github.com/gombit-dev/gombit/storage/claims.Claim",
		"gombit db migrate",
		"never applies a migration",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps lack %q:\n%s", want, steps)
		}
	}
	var out strings.Builder
	if err := r.Render(&out); err != nil || !strings.Contains(out.String(), "[migration required] `auth.RefreshToken` (`refresh_tokens`): altered (v0.9.0, schema-v0-9-0)") || !strings.Contains(out.String(), "Required migration steps (the upgrade is not complete until they are done):\n  1. ") {
		t.Fatalf("render:\n%s (%v)", out.String(), err)
	}

	// Not registered: the app has no such table; nothing to migrate.
	r, err = upgrade.AnalyzeSchema(registryApp(t, claimModel), "", []upgrade.Release{schemaRelease("v0.9.0", "manual", refresh)})
	if err != nil || r.MigrationRequired() || r.Findings[0].Required || !strings.Contains(r.Findings[0].Advice, "no refresh_tokens to migrate") {
		t.Fatalf("an altered table the app lacks = %+v, %v", r, err)
	}
	// Informational (an optional feature's table): nothing required.
	r, err = upgrade.AnalyzeSchema(registryApp(t, authRefresh), "", []upgrade.Release{schemaRelease("v0.7.0", "informational", claim)})
	if err != nil || r.MigrationRequired() || !strings.Contains(r.Findings[0].Advice, "unless the application adopts it") {
		t.Fatalf("an optional table = %+v, %v", r, err)
	}
	// Removed and registered: forget it, destructive.
	gone := upgrade.SchemaChange{Model: "auth.RefreshToken", Table: "refresh_tokens", Change: upgrade.SchemaRemoved, Destructive: true}
	r, err = upgrade.AnalyzeSchema(registryApp(t, authRefresh), "", []upgrade.Release{schemaRelease("v1.0.0", "manual", gone)})
	if err != nil || !r.MigrationRequired() || !strings.Contains(strings.Join(r.Steps, "\n"), "--forget-model github.com/gombit-dev/gombit/auth.RefreshToken") || !strings.Contains(r.Findings[0].Advice, "Destructive") {
		t.Fatalf("a removed table = %+v, %v", r, err)
	}
	// No registry at all: the app has no migrations; say so first. Which
	// framework tables it has is unknown, so an altered one needs a
	// migration, not "not its table".
	r, err = upgrade.AnalyzeSchema(registryApp(t), "", []upgrade.Release{schemaRelease("v0.9.0", "manual", refresh)})
	if err != nil || r.RegistryFound || !r.Findings[0].Required || !strings.Contains(r.Steps[0], "has no model registry") || !strings.Contains(r.Findings[0].Advice, "AutoMigrate's call") {
		t.Fatalf("no registry = %+v, %v", r, err)
	}
	// Later changes see the registry as the earlier steps leave it: a
	// model added on the path, then altered, needs no second "not its
	// table".
	claimAltered := upgrade.SchemaChange{Model: "storage/claims.Claim", Table: "storage_claims", Change: upgrade.SchemaAltered}
	r, err = upgrade.AnalyzeSchema(registryApp(t, authRefresh), "", []upgrade.Release{schemaRelease("v0.9.0", "manual", claim), schemaRelease("v0.10.0", "manual", claimAltered)})
	if err != nil || len(r.Findings) != 2 || !r.Findings[1].Required || !r.Findings[1].Registered {
		t.Fatalf("added, then altered = %+v, %v", r, err)
	}
	// No schema change on the path: nothing.
	r, err = upgrade.AnalyzeSchema(registryApp(t, authRefresh), "", []upgrade.Release{{Version: "v0.9.0"}})
	if err != nil || len(r.Findings) != 0 || len(r.Steps) != 0 {
		t.Fatalf("no change = %+v, %v", r, err)
	}
	// A broken registry is an error, not a guess.
	broken := t.TempDir()
	writeTree(t, broken, map[string]string{"database/migrations/models.json": "{"})
	if _, err := upgrade.AnalyzeSchema(broken, "", nil); err == nil {
		t.Fatal("a registry that does not parse was accepted")
	}
}

// TestManifestRefusesBadSchemaChanges: schema entries belong to a schema
// change the developer acts on (or an optional feature's), name a framework
// model this gombit has, and say what happens.
func TestManifestRefusesBadSchemaChanges(t *testing.T) {
	entry := func(change, fields string) string {
		return "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - {id: x, " + change + ", summary: s, details: d, schema: [" + fields + "]}\n"
	}
	for name, tc := range map[string]struct{ manifest, want string }{
		"wrong area":    {entry("kind: manual, area: api", "{model: auth.User, table: users, change: altered}"), "its area is schema"},
		"automatic":     {entry("kind: automatic, action: record-upgrade-baseline, area: schema", "{model: auth.User, table: users, change: altered}"), "not automatic"},
		"bad model":     {entry("kind: manual, area: schema", "{model: User, table: users, change: added}"), "want a framework package and type"},
		"bad change":    {entry("kind: manual, area: schema", "{model: auth.User, table: users, change: renamed}"), "want added, altered or removed"},
		"twice":         {entry("kind: manual, area: schema", "{model: auth.User, table: users, change: altered}, {model: auth.User, table: users, change: altered}"), "listed twice"},
		"destroy added": {entry("kind: manual, area: schema", "{model: auth.User, table: users, change: added, destructive: true}"), "not destructive"},
	} {
		if _, err := upgrade.ParseManifest([]byte(tc.manifest)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	ok := entry("kind: manual, area: schema", "{model: auth.User, table: users, change: altered}, {model: auth.Gone, table: gones, change: removed}")
	if _, err := upgrade.ParseManifest([]byte(ok)); err != nil {
		t.Fatalf("a valid schema change: %v", err)
	}
}

// TestManifestSchemaModelsAreFrameworkModels: every schema entry names a
// framework model this gombit has, unless a later release declares it
// removed. The parser does not check it: an entry of a published release
// must stay valid after its model is gone.
func TestManifestSchemaModelsAreFrameworkModels(t *testing.T) {
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	removedLater := map[string]bool{}
	for i := len(m.Releases) - 1; i >= 0; i-- {
		for _, c := range m.Releases[i].Changes {
			for _, sc := range c.Schema {
				if sc.Change == upgrade.SchemaRemoved {
					removedLater[sc.Model] = true
					continue
				}
				if !removedLater[sc.Model] && !slices.Contains(upgrade.FrameworkModels, sc.RegistryModel()) {
					t.Errorf("release %s, change %s: %s is not in upgrade.FrameworkModels, and no later release removes it", m.Releases[i].Version, c.ID, sc.Model)
				}
			}
		}
	}
}
