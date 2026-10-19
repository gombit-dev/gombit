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
	Table string `json:"table"`
	Model string `json:"model"`
	// Join: a many-to-many join table Model declares.
	Join        bool               `json:"join,omitempty"`
	Columns     []schemaColumn     `json:"columns"`
	Indexes     []schemaIndex      `json:"indexes"`
	Constraints []schemaConstraint `json:"constraints"`
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
	Name    string              `json:"name"`
	Class   string              `json:"class,omitempty"` // UNIQUE, FULLTEXT, SPATIAL
	Type    string              `json:"type,omitempty"`  // btree, hash, gin...
	Where   string              `json:"where,omitempty"`
	Option  string              `json:"option,omitempty"`
	Columns []schemaIndexColumn `json:"columns"`
}

type schemaIndexColumn struct {
	Name       string `json:"name,omitempty"`
	Expression string `json:"expression,omitempty"`
	Sort       string `json:"sort,omitempty"`
	Collate    string `json:"collate,omitempty"`
	Length     int    `json:"length,omitempty"`
}

// schemaConstraint is a foreign key (its columns, the table and columns it
// references, its delete and update rules) or a CHECK constraint.
type schemaConstraint struct {
	Name       string   `json:"name"`
	Check      string   `json:"check,omitempty"`
	Columns    []string `json:"columns,omitempty"`
	References string   `json:"references,omitempty"`
	OnDelete   string   `json:"on_delete,omitempty"`
	OnUpdate   string   `json:"on_update,omitempty"`
}

// frameworkModels are the framework's own Models(), the ones the catalog
// stands for.
func frameworkModels() []any { return append(auth.Models(), claims.Models()...) }

// currentFrameworkSchema is the framework-owned schema the code defines, as
// GORM (and so the Atlas GORM provider) reads the models: each model's
// table and each many-to-many join table it declares, with their columns,
// indexes (every attribute GORM reads), foreign keys (with their rules)
// and CHECK constraints.
func currentFrameworkSchema(t *testing.T) map[string]schemaTable {
	t.Helper()
	cache := &sync.Map{}
	tables := map[string]schemaTable{}
	var parsed []*schema.Schema
	for _, m := range frameworkModels() {
		s, err := schema.Parse(m, cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		typ := reflect.TypeOf(m).Elem()
		model := strings.TrimPrefix(typ.PkgPath(), "github.com/gombit-dev/gombit/") + "." + typ.Name()
		add := func(s *schema.Schema, join bool) {
			st := schemaTable{Table: s.Table, Model: model, Join: join, Columns: []schemaColumn{}, Indexes: []schemaIndex{}, Constraints: []schemaConstraint{}}
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
				si := schemaIndex{Name: idx.Name, Class: idx.Class, Type: idx.Type, Where: idx.Where, Option: idx.Option, Columns: []schemaIndexColumn{}}
				for _, o := range idx.Fields {
					c := schemaIndexColumn{Expression: o.Expression, Sort: o.Sort, Collate: o.Collate, Length: o.Length}
					if o.Field != nil {
						c.Name = o.DBName
					}
					si.Columns = append(si.Columns, c)
				}
				st.Indexes = append(st.Indexes, si)
			}
			slices.SortFunc(st.Indexes, func(a, b schemaIndex) int { return strings.Compare(a.Name, b.Name) })
			for _, chk := range s.ParseCheckConstraints() {
				st.Constraints = append(st.Constraints, schemaConstraint{Name: chk.Name, Check: chk.Constraint})
			}
			tables[st.Table] = st
			parsed = append(parsed, s)
		}
		add(s, false)
		for _, rel := range s.Relationships.Many2Many {
			add(rel.JoinTable, true)
		}
	}
	// Foreign keys, on the table that holds them, as the migrator creates
	// them from each schema's relationships.
	for _, s := range parsed {
		for _, rel := range s.Relationships.Relations {
			// As the migrator: a schema creates the constraints that are
			// its own (a many-to-many's are its join table's).
			c := rel.ParseConstraint()
			if c == nil || c.Schema != s {
				continue
			}
			st, ok := tables[c.Schema.Table]
			if !ok || slices.ContainsFunc(st.Constraints, func(x schemaConstraint) bool { return x.Name == c.Name }) {
				continue
			}
			sc := schemaConstraint{Name: c.Name, OnDelete: c.OnDelete, OnUpdate: c.OnUpdate}
			for _, f := range c.ForeignKeys {
				sc.Columns = append(sc.Columns, f.DBName)
			}
			var refs []string
			for _, f := range c.References {
				refs = append(refs, f.DBName)
			}
			sc.References = c.ReferenceSchema.Table + "(" + strings.Join(refs, ",") + ")"
			st.Constraints = append(st.Constraints, sc)
			tables[c.Schema.Table] = st
		}
	}
	for name, st := range tables {
		slices.SortFunc(st.Constraints, func(a, b schemaConstraint) int { return strings.Compare(a.Name, b.Name) })
		tables[name] = st
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
	declared := map[string][]upgrade.SchemaChange{}
	after := false
	for _, r := range m.Releases {
		if after {
			for _, c := range r.Changes {
				for _, sc := range c.Schema {
					declared[sc.Table] = append(declared[sc.Table], sc)
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
// changes declared for each table since. A change that loses data (a table
// or column gone, a column's type changed other than widened) must be
// declared destructive, and a join table's entries must say join, as the
// planner reads them.
func schemaProblems(old, current map[string]schemaTable, declared map[string][]upgrade.SchemaChange) []string {
	var problems []string
	has := func(table string, k upgrade.SchemaChangeKind) bool {
		return slices.ContainsFunc(declared[table], func(sc upgrade.SchemaChange) bool { return sc.Change == k })
	}
	declare := func(st schemaTable, k upgrade.SchemaChangeKind, destructive bool) string {
		entry := fmt.Sprintf("{model: %s, table: %s", st.Model, st.Table)
		if st.Join {
			entry += ", join: true"
		}
		entry += fmt.Sprintf(", change: %s", k)
		if destructive {
			entry += ", destructive: true"
		}
		return fmt.Sprintf("under unreleased in upgrade/manifest.yaml, a manual change in the schema area with\n        schema:\n          - %s}", entry)
	}
	for _, table := range sortedTables(current) {
		cur := current[table]
		prev, existed := old[table]
		lossy := existed && dataLoss(prev, cur)
		switch {
		case !existed && !has(table, upgrade.SchemaAdded):
			problems = append(problems, fmt.Sprintf("framework table %s (%s) is new since the snapshot; declare it %s", table, cur.Model, declare(cur, upgrade.SchemaAdded, false)))
		case existed && !reflect.DeepEqual(prev, cur) && !has(table, upgrade.SchemaAltered) && !has(table, upgrade.SchemaAdded):
			problems = append(problems, fmt.Sprintf("framework table %s (%s) changed since the snapshot (%s); declare it %s", table, cur.Model, tableDiff(prev, cur), declare(cur, upgrade.SchemaAltered, lossy)))
		case existed && reflect.DeepEqual(prev, cur) && (has(table, upgrade.SchemaAltered) || has(table, upgrade.SchemaAdded)) && !has(table, upgrade.SchemaRemoved):
			problems = append(problems, fmt.Sprintf("framework table %s is declared %v, but it is as the snapshot has it: the declaration describes no change", table, kindsOf(declared[table])))
		case lossy && !slices.ContainsFunc(declared[table], func(sc upgrade.SchemaChange) bool { return sc.Destructive }):
			problems = append(problems, fmt.Sprintf("framework table %s (%s) loses data since the snapshot (%s); declare the change destructive: true", table, cur.Model, tableDiff(prev, cur)))
		}
		if has(table, upgrade.SchemaRemoved) && !has(table, upgrade.SchemaAdded) {
			problems = append(problems, fmt.Sprintf("framework table %s is declared removed, but the framework still defines it (%s)", table, cur.Model))
		}
		for _, sc := range declared[table] {
			if sc.Join != cur.Join || sc.Model != cur.Model {
				problems = append(problems, fmt.Sprintf("framework table %s is declared as {model: %s, join: %v}, but it is %s's (join: %v): the planner would misread it", table, sc.Model, sc.Join, cur.Model, cur.Join))
			}
		}
	}
	for _, table := range sortedTables(old) {
		if _, ok := current[table]; ok {
			continue
		}
		switch {
		case !has(table, upgrade.SchemaRemoved):
			problems = append(problems, fmt.Sprintf("framework table %s (%s) is gone since the snapshot; declare it %s", table, old[table].Model, declare(old[table], upgrade.SchemaRemoved, true)))
		case !slices.ContainsFunc(declared[table], func(sc upgrade.SchemaChange) bool { return sc.Destructive }):
			problems = append(problems, fmt.Sprintf("framework table %s is removed, which drops its data; declare it destructive: true", table))
		}
		for _, sc := range declared[table] {
			if sc.Join != old[table].Join || sc.Model != old[table].Model {
				problems = append(problems, fmt.Sprintf("framework table %s is declared as {model: %s, join: %v}, but it was %s's (join: %v): the planner would misread it", table, sc.Model, sc.Join, old[table].Model, old[table].Join))
			}
		}
	}
	for table := range declared {
		if _, ok := current[table]; !ok && (!has(table, upgrade.SchemaAdded) || !has(table, upgrade.SchemaRemoved)) {
			if _, was := old[table]; !was {
				problems = append(problems, fmt.Sprintf("framework table %s is declared %v, but no framework model defines it, now or in the snapshot", table, kindsOf(declared[table])))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func kindsOf(changes []upgrade.SchemaChange) []upgrade.SchemaChangeKind {
	var out []upgrade.SchemaChangeKind
	for _, sc := range changes {
		out = append(out, sc.Change)
	}
	return out
}

// dataLoss reports whether moving from prev to cur loses data: a column
// gone, or one whose type changed other than by widening its size.
func dataLoss(prev, cur schemaTable) bool {
	now := map[string]schemaColumn{}
	for _, c := range cur.Columns {
		now[c.Name] = c
	}
	for _, c := range prev.Columns {
		n, ok := now[c.Name]
		if !ok {
			return true
		}
		if n.Type != c.Type && !widened(c.Type, n.Type) {
			return true
		}
	}
	return false
}

// widened reports whether type to is from with a larger size ("string(64)"
// to "string(255)", or to no size limit).
func widened(from, to string) bool {
	base := func(t string) (string, int) {
		name, size, ok := strings.Cut(strings.TrimSuffix(t, ")"), "(")
		if !ok || strings.Contains(size, ",") {
			return t, 0
		}
		n := 0
		_, _ = fmt.Sscanf(size, "%d", &n)
		return name, n
	}
	fb, fn := base(from)
	tb, tn := base(to)
	return fb == tb && fn > 0 && (tn == 0 || tn >= fn)
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
// changed, indexes and constraints.
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
	if !reflect.DeepEqual(prev.Constraints, cur.Constraints) {
		parts = append(parts, "constraints changed")
	}
	if prev.Model != cur.Model || prev.Join != cur.Join {
		parts = append(parts, "owner changed")
	}
	return strings.Join(parts, ", ")
}

// TestSchemaProblems: the guard's rules, on fixed tables.
func TestSchemaProblems(t *testing.T) {
	a := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}, {Name: "s", Type: "string(64)"}}}
	a2 := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}, {Name: "s", Type: "string(64)"}, {Name: "x", Type: "string"}}}
	aWider := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}, {Name: "s", Type: "string(255)"}}}
	aNarrower := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}, {Name: "s", Type: "string(16)"}}}
	aDropped := schemaTable{Table: "a", Model: "auth.A", Columns: []schemaColumn{{Name: "id", Type: "uint", PrimaryKey: true}}}
	aChecked := schemaTable{Table: "a", Model: "auth.A", Columns: a.Columns, Constraints: []schemaConstraint{{Name: "chk_a_s", Check: "s <> ''"}}}
	b := schemaTable{Table: "b", Model: "auth.B", Columns: []schemaColumn{}}
	j := schemaTable{Table: "a_bs", Model: "auth.A", Join: true, Columns: []schemaColumn{{Name: "a_id", Type: "uint", PrimaryKey: true}}}
	tables := func(ts ...schemaTable) map[string]schemaTable {
		out := map[string]schemaTable{}
		for _, st := range ts {
			out[st.Table] = st
		}
		return out
	}
	// decl: table, change, then optional "destructive" / "join" flags,
	// entries separated by ";".
	decl := func(spec string) map[string][]upgrade.SchemaChange {
		out := map[string][]upgrade.SchemaChange{}
		if spec == "" {
			return out
		}
		for _, e := range strings.Split(spec, ";") {
			f := strings.Fields(e)
			sc := upgrade.SchemaChange{Table: f[0], Change: upgrade.SchemaChangeKind(f[1]), Model: map[string]string{"a": "auth.A", "b": "auth.B", "a_bs": "auth.A", "zz": "auth.Z"}[f[0]]}
			sc.Destructive = slices.Contains(f, "destructive")
			sc.Join = slices.Contains(f, "join")
			out[sc.Table] = append(out[sc.Table], sc)
		}
		return out
	}
	for name, tc := range map[string]struct {
		old, cur map[string]schemaTable
		declared string
		want     string
	}{
		"unchanged":                 {tables(a), tables(a), "", ""},
		"new table":                 {tables(a), tables(a, b), "", "framework table b (auth.B) is new"},
		"new table, declared":       {tables(a), tables(a, b), "b added", ""},
		"altered":                   {tables(a), tables(a2), "", "column x added"},
		"altered, declared":         {tables(a), tables(a2), "a altered", ""},
		"widened, not destructive":  {tables(a), tables(aWider), "a altered", ""},
		"narrowed, not destructive": {tables(a), tables(aNarrower), "a altered", "declare the change destructive: true"},
		"narrowed, destructive":     {tables(a), tables(aNarrower), "a altered destructive", ""},
		"column dropped":            {tables(a), tables(aDropped), "a altered", "loses data"},
		"check added":               {tables(a), tables(aChecked), "", "constraints changed"},
		"gone":                      {tables(a, b), tables(a), "", "framework table b (auth.B) is gone"},
		"gone, not destructive":     {tables(a, b), tables(a), "b removed", "declare it destructive: true"},
		"gone, declared":            {tables(a, b), tables(a), "b removed destructive", ""},
		"join gone, as a join":      {tables(a, j), tables(a), "a_bs removed destructive join", ""},
		"join gone, as a model":     {tables(a, j), tables(a), "a_bs removed destructive", "the planner would misread it"},
		"join new, paste says join": {tables(a), tables(a, j), "", "join: true, change: added"},
		"altered, but no change":    {tables(a), tables(a), "a altered", "describes no change"},
		"removed, still there":      {tables(a), tables(a), "a removed", "still defines it"},
		"declared, no such table":   {tables(a), tables(a), "zz added", "no framework model defines it"},
		"added, then removed":       {tables(a), tables(a), "zz added;zz removed destructive", ""},
	} {
		got := strings.Join(schemaProblems(tc.old, tc.cur, decl(tc.declared)), "\n")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", name, got, tc.want)
		}
	}
}

// TestFrameworkTablesAreTheModelsOwn: a schema entry's model and table
// must match the framework's models as GORM reads them: the planner reads
// an entry on a model's own table as the model (added or removed), and one
// on a join table (join: true) as a change to the model. Every manifest
// entry for a model the framework still has says which, truthfully.
func TestFrameworkTablesAreTheModelsOwn(t *testing.T) {
	current := currentFrameworkSchema(t)
	m, err := upgrade.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Releases {
		for _, c := range r.Changes {
			for _, sc := range c.Schema {
				st, ok := current[sc.Table]
				if !ok {
					continue // gone since: TestFrameworkSchemaIsDeclared holds it to the snapshot
				}
				if st.Model != sc.Model || st.Join != sc.Join {
					t.Errorf("release %s, change %s: %s is declared {model: %s, join: %v}, but it is %s's (join: %v)", r.Version, c.ID, sc.Table, sc.Model, sc.Join, st.Model, st.Join)
				}
			}
		}
	}
	for _, st := range current {
		if !st.Join && upgrade.FrameworkTableForTest(st.Model) != st.Table {
			t.Errorf("%s's own table is %s, but the catalog says %q", st.Model, st.Table, upgrade.FrameworkTableForTest(st.Model))
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

// TestAnalyzeSchemaJoinTables: a join table's entry alters its model, never
// adds or removes it. When the framework drops auth.Permission (its own
// table and the join tables auth.User and auth.Group declare for it), only
// auth.Permission is forgotten: users and groups are altered, and a later
// change to users still finds the model registered.
func TestAnalyzeSchemaJoinTables(t *testing.T) {
	model := func(typ string) migrations.Model {
		return migrations.Model{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: typ}
	}
	dir := registryApp(t, model("User"), model("Group"), model("Permission"))
	r, err := upgrade.AnalyzeSchema(dir, "", []upgrade.Release{
		schemaRelease("v1.0.0", "manual",
			upgrade.SchemaChange{Model: "auth.Permission", Table: "permissions", Change: upgrade.SchemaRemoved, Destructive: true},
			upgrade.SchemaChange{Model: "auth.User", Table: "auth_user_permissions", Join: true, Change: upgrade.SchemaRemoved, Destructive: true},
			upgrade.SchemaChange{Model: "auth.Group", Table: "auth_group_permissions", Join: true, Change: upgrade.SchemaRemoved, Destructive: true},
		),
		schemaRelease("v1.1.0", "manual", upgrade.SchemaChange{Model: "auth.User", Table: "users", Change: upgrade.SchemaAltered}),
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Join(r.Steps, "\n")
	if !strings.Contains(steps, "--forget-model github.com/gombit-dev/gombit/auth.Permission") || strings.Contains(steps, "auth.User") || strings.Contains(steps, "auth.Group") {
		t.Fatalf("steps forget the wrong models:\n%s", steps)
	}
	for _, f := range r.Findings[1:3] {
		if !f.Required || !strings.Contains(f.Advice, "carries its removed join table") {
			t.Errorf("join table finding = %+v", f)
		}
	}
	if users := r.Findings[3]; !users.Required || !users.Registered {
		t.Fatalf("users after the join tables went: %+v", users)
	}
	var out strings.Builder
	_ = r.Render(&out)
	if !strings.Contains(out.String(), "`auth.User` (join table `auth_user_permissions`): removed, destructive") {
		t.Fatalf("render:\n%s", out.String())
	}
}

// TestAnalyzeSchemaStepsAgree: the review, generate and apply steps carry
// one set of flags (the migration directory, the models added and
// forgotten), so the plan reviewed is the migration generated, in the
// application's own directory; a destructive change says --allow.
func TestAnalyzeSchemaStepsAgree(t *testing.T) {
	dir := t.TempDir()
	migrationDir := filepath.Join(dir, "db", "migrations")
	if err := os.MkdirAll(migrationDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := migrations.SaveRegistry(migrationDir, []migrations.Model{{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "RefreshToken"}}); err != nil {
		t.Fatal(err)
	}
	r, err := upgrade.AnalyzeSchema(dir, "db/migrations", []upgrade.Release{schemaRelease("v0.9.0", "manual",
		upgrade.SchemaChange{Model: "storage/claims.Claim", Table: "storage_claims", Change: upgrade.SchemaAdded},
		upgrade.SchemaChange{Model: "auth.RefreshToken", Table: "refresh_tokens", Change: upgrade.SchemaAltered, Destructive: true},
	)})
	if err != nil {
		t.Fatal(err)
	}
	flags := " --dir db/migrations --model github.com/gombit-dev/gombit/storage/claims.Claim"
	steps := strings.Join(r.Steps, "\n")
	for _, want := range []string{
		"gombit db plan" + flags + ".",
		"gombit db makemigrations upgrade_framework_schema" + flags + ".",
		"This upgrade declares a destructive change",
		"--allow <its id>",
		"gombit db migrate --dir db/migrations,",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps lack %q:\n%s", want, steps)
		}
	}
	if r.Registry != "db/migrations/models.json" || !r.RegistryFound {
		t.Fatalf("registry = %q, %v", r.Registry, r.RegistryFound)
	}
}

// TestInformationalSchemaChangesOnlyAdd: an informational change may only
// add an optional feature's table; altering or removing one an application
// may have needs a migration, so the change must be manual.
func TestInformationalSchemaChangesOnlyAdd(t *testing.T) {
	for _, entry := range []string{
		"{model: auth.User, table: users, change: altered}",
		"{model: auth.User, table: users, change: removed, destructive: true}",
		"{model: auth.User, table: auth_user_groups, join: true, change: added}",
	} {
		manifest := "format: 1\nreleases:\n  - version: v0.1.0\n  - version: v0.2.0\n    changes:\n      - {id: x, kind: informational, area: schema, summary: s, schema: [" + entry + "]}\n"
		if _, err := upgrade.ParseManifest([]byte(manifest)); err == nil || !strings.Contains(err.Error(), "so the change is manual") {
			t.Errorf("informational %s: %v", entry, err)
		}
	}
}

// TestAnalyzeSchemaAddedThenRemoved: a model a release adds and a later one
// on the path removes never reaches the application: nothing to add,
// forget or migrate.
func TestAnalyzeSchemaAddedThenRemoved(t *testing.T) {
	device := func(k upgrade.SchemaChangeKind, destructive bool) upgrade.SchemaChange {
		return upgrade.SchemaChange{Model: "auth.Device", Table: "devices", Change: k, Destructive: destructive}
	}
	r, err := upgrade.AnalyzeSchema(registryApp(t, migrations.Model{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "User"}), "", []upgrade.Release{
		schemaRelease("v1.1.0", "manual", device(upgrade.SchemaAdded, false)),
		schemaRelease("v1.2.0", "manual", device(upgrade.SchemaRemoved, true)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.MigrationRequired() || r.Findings[0].Required || r.Findings[1].Required || strings.Contains(r.Findings[1].Advice, "Destructive") {
		t.Fatalf("added, then removed = %+v", r)
	}
	if !strings.Contains(r.Findings[0].Advice, "comes and goes on this path") {
		t.Fatalf("advice = %q", r.Findings[0].Advice)
	}
}

// TestAnalyzeSchemaJoinWithoutTheFlag: an entry on a table that is not the
// own table of a model this gombit has is read as a join table even
// without join: true, so the planner never forgets a model the framework
// still has.
func TestAnalyzeSchemaJoinWithoutTheFlag(t *testing.T) {
	r, err := upgrade.AnalyzeSchema(registryApp(t, migrations.Model{ImportPath: "github.com/gombit-dev/gombit/auth", TypeName: "User"}), "", []upgrade.Release{
		schemaRelease("v1.0.0", "manual", upgrade.SchemaChange{Model: "auth.User", Table: "auth_user_permissions", Change: upgrade.SchemaRemoved, Destructive: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if steps := strings.Join(r.Steps, "\n"); strings.Contains(steps, "--forget-model") || !r.Findings[0].Required {
		t.Fatalf("a join table without the flag = %+v", r)
	}
}
