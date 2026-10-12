package upgrade_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/upgrade"
)

// codemodFramework is a module at the framework's path with old APIs and
// their replacements side by side, as a release deprecating one ships both.
var codemodFramework = map[string]string{
	"go.mod": "module github.com/gombit-dev/gombit\n\ngo 1.21\n",
	"db/db.go": `package db

// Old opens a store.
//
// Deprecated: use Open.
func Old(name string, n int) *Store { return &Store{} }

// Open opens a store.
func Open(name string, n int) *Store { return &Store{} }

// Connect opens a store, its arguments the other way around.
func Connect(n int, name string) *Store { return &Store{} }

// Store is a store.
type Store struct {
	Name  string
	Label string
}

// Get gets.
func (s *Store) Get() {}

// Fetch fetches.
func (s *Store) Fetch() {}

// Peek peeks.
func (s Store) Peek() {}

// Look looks, through a pointer.
func (s *Store) Look() {}

// Legacy is the old store.
type Legacy struct{ ID int }

// Modern is the new one.
type Modern struct{ ID int }

const (
	Max   = 10
	Limit = 10
)

// Wide has Old directly and New through Inner.
type Wide struct {
	Old int
	Inner
}

// Inner is embedded in Wide.
type Inner struct{ New int }

// Pair returns v, named.
func Pair[K comparable, V any](name K, v V) V { return v }

// Pairs is Pair, renamed.
func Pairs[K comparable, V any](name K, v V) V { return v }

// Swap is Pair, its arguments the other way around.
func Swap[K comparable, V any](v V, name K) V { return v }

// Table holds openers.
var Table = map[string]func(string, int) *Store{}

// Base has a method.
type Base struct{}

// Hello says hello.
func (Base) Hello() {}

// Wrapper embeds Base, so Base's Hello is promoted through it.
type Wrapper struct{ Base }

// Greeter greets.
type Greeter interface{ Hello() }

// Loud embeds Greeter.
type Loud interface {
	Greeter
	Shout()
}
`,
	"store/store.go": `package store

import "github.com/gombit-dev/gombit/db"

// Open opens a store.
func Open(name string, n int) *db.Store { return db.Open(name, n) }
`,
}

const (
	dbPath    = "github.com/gombit-dev/gombit/db"
	storePath = "github.com/gombit-dev/gombit/store"
)

// codemodApp is an application using codemodFramework, with files.
func codemodApp(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	writeTree(t, filepath.Join(root, "fw"), codemodFramework)
	app := filepath.Join(root, "app")
	all := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => ../fw\n",
	}
	for k, v := range files {
		all[k] = v
	}
	writeTree(t, app, all)
	return app
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// renames are a codemod of each kind of API a rename covers.
var renames = []upgrade.Codemod{
	{ID: "old-to-open", Description: "db.Old is db.Open", Package: dbPath, Symbol: "Old", Rename: "Open"},
	{ID: "get-to-fetch", Description: "Store.Get is Store.Fetch", Package: dbPath, Symbol: "Store.Get", Rename: "Fetch"},
	{ID: "name-to-label", Description: "Store.Name is Store.Label", Package: dbPath, Symbol: "Store.Name", Rename: "Label"},
	{ID: "legacy-to-modern", Description: "db.Legacy is db.Modern", Package: dbPath, Symbol: "Legacy", Rename: "Modern"},
	{ID: "max-to-limit", Description: "db.Max is db.Limit", Package: dbPath, Symbol: "Max", Rename: "Limit"},
}

const renameApp = `package main

import "github.com/gombit-dev/gombit/db"

// Old is a local name: db.Old in a comment or a string is not a use.
func Old() {}

func main() {
	Old()
	_ = "db.Old()"
	s := db.Old("a", 1) // keeps this comment
	s.Get()
	get := s.Get
	get()
	(*db.Store).Get(s)
	_ = s.Name
	_ = db.Store{Name: "x"}
	_ = db.Legacy{ID: 1}
	var l *db.Legacy
	_ = l
	_ = db.Max + db.Limit
}
`

const renamedApp = `package main

import "github.com/gombit-dev/gombit/db"

// Old is a local name: db.Old in a comment or a string is not a use.
func Old() {}

func main() {
	Old()
	_ = "db.Old()"
	s := db.Open("a", 1) // keeps this comment
	s.Fetch()
	get := s.Fetch
	get()
	(*db.Store).Fetch(s)
	_ = s.Label
	_ = db.Store{Label: "x"}
	_ = db.Modern{ID: 1}
	var l *db.Modern
	_ = l
	_ = db.Limit + db.Limit
}
`

// TestRunCodemodsRename: a rename rewrites every use of the API (a call, a
// method value and expression, a field, a composite literal key, a type, a
// constant), found by type information: a local name, a comment or a string
// is not a use. The output is gofmt-formatted, nothing is written until
// Write, and a second run proposes nothing.
func TestRunCodemodsRename(t *testing.T) {
	app := codemodApp(t, map[string]string{
		"main.go":      renameApp,
		"main_test.go": "package main\n\nimport (\n\t\"testing\"\n\n\t\"github.com/gombit-dev/gombit/db\"\n)\n\nfunc TestOpen(t *testing.T) { db.Old(\"t\", 0).Get() }\n",
	})
	fix, err := upgrade.RunCodemods(context.Background(), app, renames)
	if err != nil {
		t.Fatal(err)
	}
	if len(fix.Left) != 0 || len(fix.Unchecked) != 0 {
		t.Fatalf("left %v, unchecked %v", fix.Left, fix.Unchecked)
	}
	if len(fix.Files) != 2 || fix.Files[0].Path != "main.go" || fix.Files[1].Path != "main_test.go" {
		t.Fatalf("files = %+v", fix.Files)
	}
	if got := string(fix.Files[0].After); got != renamedApp {
		t.Fatalf("main.go:\n%s\nwant:\n%s", got, renamedApp)
	}
	if got := string(fix.Files[1].After); !strings.Contains(got, `db.Open("t", 0).Fetch()`) {
		t.Fatalf("main_test.go:\n%s", got)
	}
	// Proposed, not written.
	if got := read(t, filepath.Join(app, "main.go")); got != renameApp {
		t.Fatal("RunCodemods wrote main.go")
	}
	patch := fix.Patch()
	if !strings.HasPrefix(patch, "--- a/main.go\n+++ b/main.go\n@@ ") || !strings.Contains(patch, "\n-\ts.Get()\n") || !strings.Contains(patch, "\n+\ts.Fetch()\n") {
		t.Fatalf("patch:\n%s", patch)
	}

	written, err := fix.Write()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, ",") != "main.go,main_test.go" {
		t.Fatalf("written = %v", written)
	}
	if got := read(t, filepath.Join(app, "main.go")); got != renamedApp {
		t.Fatalf("written main.go:\n%s", got)
	}
	again, err := upgrade.RunCodemods(context.Background(), app, renames)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Files) != 0 || len(again.Left) != 0 || again.Patch() != "" {
		t.Fatalf("second run: %+v", again)
	}
}

// TestRunCodemodsPatchApplies: the patch applies with git apply, to the
// same result Write gives.
func TestRunCodemodsPatchApplies(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	app := codemodApp(t, map[string]string{"main.go": renameApp})
	fix, err := upgrade.RunCodemods(context.Background(), app, renames)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "core.autocrlf=false", "apply", "-") // not the host's line-ending conversion
	cmd.Dir = app
	cmd.Stdin = strings.NewReader(fix.Patch())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s", err, out)
	}
	if got := read(t, filepath.Join(app, "main.go")); got != renamedApp {
		t.Fatalf("applied:\n%s", got)
	}
}

// TestRunCodemodsMove: a move qualifies each use with the new package,
// importing it, and drops the old import where nothing else uses it; an
// import name already taken in the file gets a free one, and a use where a
// local name shadows it is left.
func TestRunCodemodsMove(t *testing.T) {
	app := codemodApp(t, map[string]string{
		"only.go": `package main

import "github.com/gombit-dev/gombit/db"

func only() { _ = db.Old("a", 1) }
`,
		"both.go": `package main

import (
	"fmt"

	"github.com/gombit-dev/gombit/db"
)

func both() { fmt.Println(db.Old("a", 1), db.Max) }
`,
		"taken.go": `package main

import (
	"github.com/gombit-dev/gombit/db"
	store "fmt"
)

func taken() { store.Println(db.Old("a", 1)) }
`,
		"shadow.go": `package main

import "github.com/gombit-dev/gombit/db"

func shadow() {
	store := 1
	_ = store
	_ = db.Old("a", 1)
}
`,
		"main.go": "package main\n\nfunc main() { only(); both(); taken(); shadow() }\n",
	})
	move := []upgrade.Codemod{{ID: "old-to-store", Description: "db.Old is store.Open", Package: dbPath, Symbol: "Old", MoveTo: storePath, Rename: "Open"}}
	fix, err := upgrade.RunCodemods(context.Background(), app, move)
	if err != nil {
		t.Fatal(err)
	}
	after := map[string]string{}
	for _, f := range fix.Files {
		after[f.Path] = string(f.After)
	}
	want := map[string]string{
		"only.go": `package main

import "github.com/gombit-dev/gombit/store"

func only() { _ = store.Open("a", 1) }
`,
		"both.go": `package main

import (
	"fmt"

	"github.com/gombit-dev/gombit/db"
	"github.com/gombit-dev/gombit/store"
)

func both() { fmt.Println(store.Open("a", 1), db.Max) }
`,
		"taken.go": `package main

import (
	store "fmt"
	store2 "github.com/gombit-dev/gombit/store"
)

func taken() { store.Println(store2.Open("a", 1)) }
`,
	}
	for name, w := range want {
		if after[name] != w {
			t.Errorf("%s:\n%s\nwant:\n%s", name, after[name], w)
		}
	}
	if _, ok := after["shadow.go"]; ok || len(fix.Files) != 3 {
		t.Fatalf("files: %v", fix.Files)
	}
	if len(fix.Left) != 1 || fix.Left[0].File != "shadow.go" || fix.Left[0].Line != 8 || !strings.Contains(fix.Left[0].Reason, "shadowed") {
		t.Fatalf("left = %+v", fix.Left)
	}
}

// TestRunCodemodsDotImport: a dot-imported use stays bare when the API
// keeps its package, and is qualified when it moves (the dot import going
// once nothing uses it).
func TestRunCodemodsDotImport(t *testing.T) {
	app := codemodApp(t, map[string]string{
		"a.go":    "package main\n\nimport . \"github.com/gombit-dev/gombit/db\"\n\nfunc a() { _ = Old(\"a\", 1) }\n",
		"main.go": "package main\n\nimport . \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { a(); c(); d(); _ = Old(\"b\", 2); _ = Max }\n",
		"d.go":    "package main\n\nimport (\n\t. \"github.com/gombit-dev/gombit/db\"\n\tdb \"github.com/gombit-dev/gombit/db\"\n)\n\nfunc d() { _ = Old(\"d\", 4); _ = db.Max }\n",
		"c.go":    "package main\n\nimport . \"github.com/gombit-dev/gombit/db\"\n\nfunc c() {\n\tOpen := 1\n\t_ = Open\n\t_ = Old(\"c\", 3)\n}\n",
	})
	fix, err := upgrade.RunCodemods(context.Background(), app, renames[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(fix.Files) != 3 || fix.Files[0].Path != "a.go" || !strings.Contains(string(fix.Files[0].After), `_ = Open("a", 1)`) {
		t.Fatalf("rename: %+v", fix.Files)
	}
	// A local Open shadows the dot-imported one there: left.
	if len(fix.Left) != 1 || fix.Left[0].File != "c.go" || !strings.Contains(fix.Left[0].Reason, "does not resolve") {
		t.Fatalf("left = %+v", fix.Left)
	}
	move := []upgrade.Codemod{{ID: "old-to-store", Description: "db.Old is store.Open", Package: dbPath, Symbol: "Old", MoveTo: storePath, Rename: "Open"}}
	fix, err = upgrade.RunCodemods(context.Background(), app, move)
	if err != nil {
		t.Fatal(err)
	}
	if want := "package main\n\nimport \"github.com/gombit-dev/gombit/store\"\n\nfunc a() { _ = store.Open(\"a\", 1) }\n"; string(fix.Files[0].After) != want {
		t.Fatalf("a.go:\n%s\nwant:\n%s", fix.Files[0].After, want)
	}
	if len(fix.Files) != 4 || fix.Files[3].Path != "main.go" {
		t.Fatalf("move: %+v", fix.Files)
	}
	// d.go imports db both ways: the qualified db.Max keeps the named
	// import, not the dot one.
	if want := "package main\n\nimport (\n\tdb \"github.com/gombit-dev/gombit/db\"\n\t\"github.com/gombit-dev/gombit/store\"\n)\n\nfunc d() { _ = store.Open(\"d\", 4); _ = db.Max }\n"; string(fix.Files[2].After) != want {
		t.Fatalf("d.go:\n%s\nwant:\n%s", fix.Files[2].After, want)
	}
	if got := string(fix.Files[3].After); !strings.Contains(got, `import (`) || !strings.Contains(got, `. "github.com/gombit-dev/gombit/db"`) || !strings.Contains(got, `store.Open("b", 2); _ = Max`) {
		t.Fatalf("main.go (keeps the dot import, Max uses it):\n%s", got)
	}
}

// reorder is a custom rewrite: db.Old(name, n) is db.Connect(n, name).
var reorder = upgrade.Codemod{
	ID: "old-to-connect", Description: "db.Old(name, n) is db.Connect(n, name)", Package: dbPath, Symbol: "Old",
	Rewrite: func(u *upgrade.Use) ast.Expr {
		if u.Call == nil || len(u.Call.Args) != 2 || u.Call.Ellipsis.IsValid() {
			return u.Skip("not a call with two arguments")
		}
		name, ok := u.Import(dbPath)
		if !ok {
			return u.Skip("db is shadowed")
		}
		call := *u.Call
		call.Fun = u.CallFun(&ast.SelectorExpr{X: &ast.Ident{NamePos: u.Expr.Pos(), Name: name}, Sel: &ast.Ident{NamePos: u.Ident.Pos(), Name: "Connect"}})
		call.Args = []ast.Expr{u.Call.Args[1], u.Call.Args[0]}
		return &call
	},
}

// TestRunCodemodsRewrite: a custom Rewrite reshapes a call, bottom-up (a
// rewritten call inside another's arguments), and leaves what it cannot
// rewrite with its reason.
func TestRunCodemodsRewrite(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": `package main

import "github.com/gombit-dev/gombit/db"

func main() {
	_ = db.Old("a", 1)
	_ = db.Old(db.Old("b", 2).Name, 3)
	_ = (db.Old)("p", 9)
	f := db.Old
	_ = f
}
`})
	fix, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{reorder})
	if err != nil {
		t.Fatal(err)
	}
	want := `package main

import "github.com/gombit-dev/gombit/db"

func main() {
	_ = db.Connect(1, "a")
	_ = db.Connect(3, db.Connect(2, "b").Name)
	_ = (db.Connect)(9, "p")
	f := db.Old
	_ = f
}
`
	if len(fix.Files) != 1 || string(fix.Files[0].After) != want {
		t.Fatalf("files: %+v\n%s", fix.Files, fix.Files[0].After)
	}
	if len(fix.Left) != 1 || fix.Left[0].Line != 9 || fix.Left[0].Reason != "not a call with two arguments" || fix.Left[0].Codemod != "old-to-connect" || fix.Left[0].Symbol != dbPath+".Old" {
		t.Fatalf("left = %+v", fix.Left)
	}
	// What is left is left on every run: idempotent.
	if _, err := fix.Write(); err != nil {
		t.Fatal(err)
	}
	again, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{reorder})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Files) != 0 || len(again.Left) != 1 {
		t.Fatalf("second run: %+v", again)
	}
}

// TestRunCodemodsLeavesUnsafeUses: a use the rewrite cannot keep correct
// is left unchanged with why: a member the new name would not resolve to
// (shadowed by the app's own, or a pointer method on a value that is not
// addressable), an embedded field whose name would change, a generated
// file, and an API this framework version does not have yet.
func TestRunCodemodsLeavesUnsafeUses(t *testing.T) {
	app := codemodApp(t, map[string]string{
		"main.go": `package main

import "github.com/gombit-dev/gombit/db"

type mine struct{ *db.Store }

func (mine) Fetch() {}

type wrap struct{ db.Legacy }

func main() {
	m := mine{&db.Store{}}
	m.Get()
	var s db.Store
	s.Peek()
	db.Store{}.Peek()
	_ = wrap{}
}
`,
		"gen.go": "// Code generated by hand. DO NOT EDIT.\n\npackage main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nvar _ = db.Max\n",
	})
	mods := []upgrade.Codemod{
		renames[1], renames[3], renames[4],
		{ID: "peek-to-look", Description: "Store.Peek is Store.Look", Package: dbPath, Symbol: "Store.Peek", Rename: "Look"},
		{ID: "old-to-later", Description: "db.Old is db.Later", Package: dbPath, Symbol: "Old", Rename: "Later"},
	}
	fix, err := upgrade.RunCodemods(context.Background(), app, mods)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, l := range fix.Left {
		reasons[l.Codemod+"@"+l.File+":"+strconv.Itoa(l.Line)] = l.Reason
	}
	for key, want := range map[string]string{
		"get-to-fetch@main.go:13":    "does not resolve",
		"peek-to-look@main.go:16":    "does not resolve",
		"legacy-to-modern@main.go:9": "embedded field",
		"max-to-limit@gen.go:7":      "generated file",
	} {
		if !strings.Contains(reasons[key], want) {
			t.Errorf("%s: reason %q, want %q (left: %v)", key, reasons[key], want, reasons)
		}
	}
	if len(fix.Left) != 4 {
		t.Errorf("left = %+v", fix.Left)
	}
	// s.Peek() on an addressable variable is rewritten.
	if len(fix.Files) != 1 || !strings.Contains(string(fix.Files[0].After), "\ts.Look()\n\tdb.Store{}.Peek()\n") {
		t.Fatalf("files: %+v", fix.Files)
	}

	// An API the app's framework version lacks: every use is left.
	app2 := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old(\"a\", 1) }\n"})
	fix, err = upgrade.RunCodemods(context.Background(), app2, mods[4:])
	if err != nil {
		t.Fatal(err)
	}
	if len(fix.Files) != 0 || len(fix.Left) != 1 || !strings.Contains(fix.Left[0].Reason, "does not exist in the framework version") {
		t.Fatalf("missing API: %+v", fix)
	}
}

// TestRunCodemodsRefusesBadRewrites: a Rewrite whose output still uses its
// API (a second run would rewrite it again), or does not type-check, is
// refused, and nothing is proposed.
func TestRunCodemodsRefusesBadRewrites(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old(\"a\", 1) }\n"})
	same := upgrade.Codemod{ID: "same", Description: "returns the use", Package: dbPath, Symbol: "Old",
		Rewrite: func(u *upgrade.Use) ast.Expr { return u.Node() }}
	if _, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{same}); err == nil || !strings.Contains(err.Error(), "still uses it") {
		t.Fatalf("RunCodemods = %v; want the self-use refused", err)
	}
	broken := upgrade.Codemod{ID: "broken", Description: "drops an argument", Package: dbPath, Symbol: "Old",
		Rewrite: func(u *upgrade.Use) ast.Expr {
			call := *u.Call
			call.Fun = &ast.SelectorExpr{X: ast.NewIdent("db"), Sel: ast.NewIdent("Open")}
			call.Args = call.Args[:1]
			return &call
		}}
	if _, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{broken}); !errors.Is(err, upgrade.ErrDoesNotLoad) || !strings.Contains(err.Error(), "the codemods' rewrite does not type-check") {
		t.Fatalf("RunCodemods = %v; want the broken rewrite refused", err)
	}
	misplaced := upgrade.Codemod{ID: "misplaced", Description: "a statement for an expression", Package: dbPath, Symbol: "Old",
		Rewrite: func(u *upgrade.Use) ast.Expr { return &ast.BadExpr{} }}
	if _, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{misplaced}); err == nil {
		t.Fatal("RunCodemods accepted a BadExpr")
	}
	if got := read(t, filepath.Join(app, "main.go")); !strings.Contains(got, "db.Old(") {
		t.Fatal("a refused run wrote the file")
	}
}

// TestFixWriteRefusesChangedFiles: Write writes nothing when a file
// changed after the codemods read it.
func TestFixWriteRefusesChangedFiles(t *testing.T) {
	app := codemodApp(t, map[string]string{
		"a.go":    "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nvar _ = db.Max\n",
		"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Max }\n",
	})
	fix, err := upgrade.RunCodemods(context.Background(), app, renames[4:])
	if err != nil {
		t.Fatal(err)
	}
	edited := "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Max + 1 }\n"
	writeTree(t, app, map[string]string{"main.go": edited})
	written, err := fix.Write()
	if !errors.Is(err, upgrade.ErrChangedSinceRun) || len(written) != 0 {
		t.Fatalf("Write = %v, %v; want ErrChangedSinceRun", written, err)
	}
	if read(t, filepath.Join(app, "a.go")) != "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nvar _ = db.Max\n" || read(t, filepath.Join(app, "main.go")) != edited {
		t.Fatal("Write changed a file")
	}
}

// TestRunCodemodsKeepsCRLF: a file with CRLF line endings keeps them, so
// the patch changes only the rewritten lines.
func TestRunCodemodsKeepsCRLF(t *testing.T) {
	src := "package main\r\n\r\nimport \"github.com/gombit-dev/gombit/db\"\r\n\r\nfunc main() {\r\n\t_ = db.Max\r\n}\r\n"
	app := codemodApp(t, map[string]string{"main.go": src})
	fix, err := upgrade.RunCodemods(context.Background(), app, renames[4:])
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(src, "db.Max", "db.Limit", 1); len(fix.Files) != 1 || string(fix.Files[0].After) != want {
		t.Fatalf("after = %q", fix.Files)
	}
	if patch := fix.Patch(); !strings.Contains(patch, "\n-\t_ = db.Max\r\n+\t_ = db.Limit\r\n }\r\n") || strings.Count(patch, "\n-\t") != 1 || strings.Count(patch, "\n+\t") != 1 {
		t.Fatalf("patch:\n%s", patch)
	}
}

// TestRunCodemodsUnchecked: a file the build configuration excludes is not
// type-checked, so not rewritten, and is listed when it mentions the API,
// also in a directory where every file is excluded (which yields no
// package to load: a platform-only directory). What ./... never matches
// (testdata, vendor, "." and "_" names, a nested module) is not listed.
func TestRunCodemodsUnchecked(t *testing.T) {
	other := "windows"
	if runtime.GOOS == "windows" {
		other = "linux"
	}
	const uses = "package %s\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nvar _ = db.Max\n"
	app := codemodApp(t, map[string]string{
		"main.go":                            "package main\n\nfunc main() {}\n",
		"used.go":                            fmt.Sprintf(uses, "main"),
		"other.go":                           "//go:build never\n\n" + fmt.Sprintf(uses, "main"),
		"quiet.go":                           "//go:build never\n\npackage main\n\nvar _ = 1\n",
		"internal/plat/api_" + other + ".go": fmt.Sprintf(uses, "plat"),
		"internal/never/a/b.go":              "//go:build never\n\n" + fmt.Sprintf(uses, "b"),
		"internal/broken/x.go":               "//go:build never\n\npackage x\n\nfunc {\n",
		"testdata/t.go":                      fmt.Sprintf(uses, "t"),
		"_skip/s.go":                         fmt.Sprintf(uses, "s"),
		".hidden/h.go":                       fmt.Sprintf(uses, "h"),
		"vendor/example.com/v/v.go":          fmt.Sprintf(uses, "v"),
		"_x.go":                              fmt.Sprintf(uses, "main"),
		"nested/go.mod":                      "module example.com/nested\n\ngo 1.21\n",
		"nested/n_" + other + ".go":          fmt.Sprintf(uses, "nested"),
	})
	fix, err := upgrade.RunCodemods(context.Background(), app, renames[4:])
	if err != nil {
		t.Fatal(err)
	}
	want := "internal/broken/x.go,internal/never/a/b.go,internal/plat/api_" + other + ".go,other.go"
	if got := strings.Join(fix.Unchecked, ","); got != want || len(fix.Files) != 1 || fix.Files[0].Path != "used.go" {
		t.Fatalf("unchecked = %s, want %s (files %v)", got, want, fix.Files)
	}
}

// TestRunCodemodsValidates: a malformed codemod is refused before any
// load.
func TestRunCodemodsValidates(t *testing.T) {
	ok := func(c upgrade.Codemod) upgrade.Codemod {
		if c.ID == "" {
			c.ID = "x"
		}
		if c.Description == "" {
			c.Description = "d"
		}
		if c.Package == "" {
			c.Package = dbPath
		}
		if c.Symbol == "" {
			c.Symbol = "Old"
		}
		return c
	}
	rewrite := func(u *upgrade.Use) ast.Expr { return nil }
	for name, c := range map[string]upgrade.Codemod{
		"bad id":           ok(upgrade.Codemod{ID: "Bad_ID", Rename: "New"}),
		"not framework":    ok(upgrade.Codemod{Package: "example.com/db", Rename: "New"}),
		"bad symbol":       ok(upgrade.Codemod{Symbol: "a.b.c", Rename: "New"}),
		"both":             ok(upgrade.Codemod{Rename: "New", Rewrite: rewrite}),
		"nothing":          ok(upgrade.Codemod{}),
		"bad rename":       ok(upgrade.Codemod{Rename: "1x"}),
		"member move":      ok(upgrade.Codemod{Symbol: "Store.Get", MoveTo: storePath}),
		"self":             ok(upgrade.Codemod{Rename: "Old"}),
		"self move":        ok(upgrade.Codemod{MoveTo: dbPath}),
		"move out":         ok(upgrade.Codemod{MoveTo: "example.com/x"}),
		"multi-line descr": ok(upgrade.Codemod{Description: "a\nb", Rename: "New"}),
	} {
		if _, err := upgrade.RunCodemods(context.Background(), t.TempDir(), []upgrade.Codemod{c}); err == nil || errors.Is(err, upgrade.ErrDoesNotLoad) {
			t.Errorf("%s: RunCodemods = %v; want it refused", name, err)
		}
	}
	dup := []upgrade.Codemod{ok(upgrade.Codemod{ID: "a", Rename: "New"}), ok(upgrade.Codemod{ID: "b", Rename: "Other"})}
	if _, err := upgrade.RunCodemods(context.Background(), t.TempDir(), dup); err == nil || !strings.Contains(err.Error(), "both rewrite") {
		t.Errorf("duplicate: RunCodemods = %v", err)
	}
}

// TestRunCodemodsRefusesAnAppThatDoesNotTypeCheck: no rewrite is proposed
// for an app that does not type-check.
func TestRunCodemodsRefusesAnAppThatDoesNotTypeCheck(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old(1) }\n"})
	if _, err := upgrade.RunCodemods(context.Background(), app, renames[:1]); !errors.Is(err, upgrade.ErrDoesNotLoad) {
		t.Fatalf("RunCodemods = %v; want ErrDoesNotLoad", err)
	}
}

// TestRunCodemodsChain: a codemod may rewrite another's output (a rename
// of a rename, as two releases ship them): the passes settle on the last
// API, and a second run changes nothing.
func TestRunCodemodsChain(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old(\"a\", 1) }\n"})
	chain := []upgrade.Codemod{
		renames[0], // db.Old -> db.Open
		{ID: "open-to-store", Description: "db.Open is store.Open", Package: dbPath, Symbol: "Open", MoveTo: storePath},
	}
	fix, err := upgrade.RunCodemods(context.Background(), app, chain)
	if err != nil {
		t.Fatal(err)
	}
	want := "package main\n\nimport \"github.com/gombit-dev/gombit/store\"\n\nfunc main() { _ = store.Open(\"a\", 1) }\n"
	if len(fix.Files) != 1 || string(fix.Files[0].After) != want || !strings.Contains(string(fix.Files[0].Before), "db.Old") {
		t.Fatalf("files: %+v", fix.Files)
	}
	if _, err := fix.Write(); err != nil {
		t.Fatal(err)
	}
	if again, err := upgrade.RunCodemods(context.Background(), app, chain); err != nil || len(again.Files) != 0 {
		t.Fatalf("second run: %+v, %v", again, err)
	}
}

// TestRunCodemodsImportNameFreeInTests: an import the rewrite adds takes a
// name free in every variant of the package, a test file's names too.
func TestRunCodemodsImportNameFreeInTests(t *testing.T) {
	app := codemodApp(t, map[string]string{
		"main.go":      "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old(\"a\", 1) }\n",
		"main_test.go": "package main\n\nvar store = 1\n",
	})
	move := []upgrade.Codemod{{ID: "old-to-store", Description: "db.Old is store.Open", Package: dbPath, Symbol: "Old", MoveTo: storePath, Rename: "Open"}}
	fix, err := upgrade.RunCodemods(context.Background(), app, move)
	if err != nil {
		t.Fatal(err)
	}
	if len(fix.Files) != 1 || !strings.Contains(string(fix.Files[0].After), `store2 "github.com/gombit-dev/gombit/store"`) {
		t.Fatalf("files: %+v", fix.Files)
	}
}

// TestRunCodemodsKeyNeedsADirectField: a composite literal key can only
// name a direct field, so a rename to a promoted one is left there (and
// done on a selector, where promotion works).
func TestRunCodemodsKeyNeedsADirectField(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() {\n\tw := db.Wide{Old: 1}\n\t_ = w.Old\n}\n"})
	mods := []upgrade.Codemod{{ID: "old-to-new", Description: "Wide.Old is Wide.New", Package: dbPath, Symbol: "Wide.Old", Rename: "New"}}
	fix, err := upgrade.RunCodemods(context.Background(), app, mods)
	if err != nil {
		t.Fatal(err)
	}
	if len(fix.Files) != 1 || !strings.Contains(string(fix.Files[0].After), "w := db.Wide{Old: 1}\n\t_ = w.New\n") {
		t.Fatalf("files: %+v", fix.Files)
	}
	if len(fix.Left) != 1 || fix.Left[0].Line != 6 || !strings.Contains(fix.Left[0].Reason, "does not resolve") {
		t.Fatalf("left = %+v", fix.Left)
	}
}

// TestRunCodemodsSnapshotsWhatItParsed: the rewrite is of the exact bytes
// go/packages parsed, never a second read. A file edited after the load,
// even to content of the same length, is not what the patch was computed
// from: Write refuses it, writing nothing. A file edited between passes
// fails the run.
func TestRunCodemodsSnapshotsWhatItParsed(t *testing.T) {
	const parsed = "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Max }\n"
	for name, edit := range map[string]string{
		"same length":  strings.Replace(parsed, "db.Max", "db.Min", 1),
		"other length": strings.Replace(parsed, "db.Max", "db.Max + 1", 1),
	} {
		t.Run(name, func(t *testing.T) {
			app := codemodApp(t, map[string]string{"main.go": parsed})
			defer upgrade.SetLoadedHookForTest(func() { writeTree(t, app, map[string]string{"main.go": edit}) })()
			fix, err := upgrade.RunCodemods(context.Background(), app, renames[4:])
			if err != nil {
				t.Fatal(err)
			}
			if len(fix.Files) != 1 || string(fix.Files[0].Before) != parsed {
				t.Fatalf("Before is not what was parsed: %+v", fix.Files)
			}
			if written, err := fix.Write(); !errors.Is(err, upgrade.ErrChangedSinceRun) || len(written) != 0 {
				t.Fatalf("Write = %v, %v; want ErrChangedSinceRun", written, err)
			}
			if read(t, filepath.Join(app, "main.go")) != edit {
				t.Fatal("Write overwrote the edit")
			}
		})
	}
	t.Run("between passes", func(t *testing.T) {
		app := codemodApp(t, map[string]string{"main.go": parsed, "other.go": "package main\n\nvar x = 1\n"})
		defer upgrade.SetLoadedHookForTest(func() { writeTree(t, app, map[string]string{"other.go": "package main\n\nvar x = 2\n"}) })()
		if _, err := upgrade.RunCodemods(context.Background(), app, renames[4:]); !errors.Is(err, upgrade.ErrChangedSinceRun) || !strings.Contains(err.Error(), "other.go") {
			t.Fatalf("RunCodemods = %v; want ErrChangedSinceRun for other.go", err)
		}
	})
}

// TestRunCodemodsRefusesAFreshSelfRewrite: a Rewrite that rebuilds its
// own target from fresh nodes (no type information, so not caught as a
// reuse), printing byte-identical source, never settles: refused, not a
// clean result.
func TestRunCodemodsRefusesAFreshSelfRewrite(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Old(\"a\", 1) }\n"})
	fresh := upgrade.Codemod{ID: "fresh", Description: "rebuilds db.Old", Package: dbPath, Symbol: "Old",
		Rewrite: func(u *upgrade.Use) ast.Expr {
			call := *u.Call
			call.Fun = &ast.SelectorExpr{X: &ast.Ident{NamePos: u.Expr.Pos(), Name: "db"}, Sel: &ast.Ident{NamePos: u.Ident.Pos(), Name: "Old"}}
			return &call
		}}
	fix, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{fresh})
	if err == nil || !strings.Contains(err.Error(), "do not settle") {
		t.Fatalf("RunCodemods = %+v, %v; want it refused", fix, err)
	}
}

// TestRunCodemodsGenericAndParenthesizedCalls: a call through parentheses
// or type arguments (one or several) is still a call: a Rewrite gets it as
// u.Call, CallFun keeps the wrappers, and a rename keeps them too. An
// instantiation that is not called is not a call.
func TestRunCodemodsGenericAndParenthesizedCalls(t *testing.T) {
	const src = `package main

import "github.com/gombit-dev/gombit/db"

func main() {
	_ = db.Pair[string, int]("a", 1)
	_ = db.Pair[string]("b", 2)
	_ = (db.Pair[string, int])("c", 3)
	_ = db.Pair("e", 5)
	f := db.Pair[string, int]
	_ = f
}
`
	swap := upgrade.Codemod{ID: "pair-to-swap", Description: "db.Pair(name, v) is db.Swap(v, name)", Package: dbPath, Symbol: "Pair",
		Rewrite: func(u *upgrade.Use) ast.Expr {
			if u.Call == nil || len(u.Call.Args) != 2 {
				return u.Skip("not a call with two arguments")
			}
			sel := u.Expr.(*ast.SelectorExpr)
			call := *u.Call
			call.Fun = u.CallFun(&ast.SelectorExpr{X: sel.X, Sel: &ast.Ident{NamePos: sel.Sel.Pos(), Name: "Swap"}})
			call.Args = []ast.Expr{u.Call.Args[1], u.Call.Args[0]}
			return &call
		}}
	app := codemodApp(t, map[string]string{"main.go": src})
	fix, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{swap})
	if err != nil {
		t.Fatal(err)
	}
	want := `package main

import "github.com/gombit-dev/gombit/db"

func main() {
	_ = db.Swap[string, int](1, "a")
	_ = db.Swap[string](2, "b")
	_ = (db.Swap[string, int])(3, "c")
	_ = db.Swap(5, "e")
	f := db.Pair[string, int]
	_ = f
}
`
	if len(fix.Files) != 1 || string(fix.Files[0].After) != want {
		t.Fatalf("swap:\n%s", fix.Files[0].After)
	}
	if len(fix.Left) != 1 || fix.Left[0].Line != 10 {
		t.Fatalf("left = %+v", fix.Left)
	}

	rename := upgrade.Codemod{ID: "pair-to-pairs", Description: "db.Pair is db.Pairs", Package: dbPath, Symbol: "Pair", Rename: "Pairs"}
	fix, err = upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{rename})
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.ReplaceAll(src, "db.Pair", "db.Pairs"); len(fix.Files) != 1 || string(fix.Files[0].After) != want || len(fix.Left) != 0 {
		t.Fatalf("rename:\n%s\nleft %+v", fix.Files[0].After, fix.Left)
	}
}

// TestRunCodemodsRefusesARewriteThatDoesNotSettle: a rewrite that brings
// back what a codemod rewrites (here two codemods undoing each other)
// never settles, and is refused.
func TestRunCodemodsRefusesARewriteThatDoesNotSettle(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Max }\n"})
	loop := []upgrade.Codemod{
		renames[4], // Max -> Limit
		{ID: "limit-to-max", Description: "db.Limit is db.Max", Package: dbPath, Symbol: "Limit", Rename: "Max"},
	}
	if _, err := upgrade.RunCodemods(context.Background(), app, loop); err == nil || !strings.Contains(err.Error(), "do not settle") {
		t.Fatalf("RunCodemods = %v; want it refused", err)
	}
}

// TestRunCodemodsIndexIsNotAnInstantiation: indexing a value and calling
// the result (db.Table["k"](x)) is not a call of the API: u.Call is nil.
func TestRunCodemodsIndexIsNotAnInstantiation(t *testing.T) {
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { _ = db.Table[\"k\"](\"a\", 1) }\n"})
	var calls []bool
	probe := upgrade.Codemod{ID: "probe", Description: "records calls", Package: dbPath, Symbol: "Table",
		Rewrite: func(u *upgrade.Use) ast.Expr {
			calls = append(calls, u.Call != nil)
			return u.Skip("probe")
		}}
	if _, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{probe}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] {
		t.Fatalf("calls = %v; want one use, not a call", calls)
	}
}

// TestCodemodActionCoverage: the codemod as a manifest action keeps the
// coverage RunCodemods reports. A use only in a file the build excludes
// (a platform-only package) makes Check report the change needed, and
// Apply fail as incomplete; with a type-checked use too, Apply writes it
// and still fails as incomplete, naming the excluded file. With every use
// rewritable and type-checked, Apply succeeds.
func TestCodemodActionCoverage(t *testing.T) {
	other := "windows"
	if runtime.GOOS == "windows" {
		other = "linux"
	}
	const uses = "package %s\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nvar _ = db.Max\n"
	excluded := "internal/plat/api_" + other + ".go"
	defer upgrade.RegisterCodemodForTest(renames[4])()
	a, ok := upgrade.LookupAction("codemod:" + renames[4].ID)
	if !ok {
		t.Fatal("LookupAction: the registered codemod is not an action")
	}
	for name, tc := range map[string]struct {
		files      map[string]string
		changed    bool
		incomplete bool
	}{
		"excluded only": {map[string]string{"main.go": "package main\n\nfunc main() {}\n", excluded: fmt.Sprintf(uses, "plat")}, false, true},
		"mixed":         {map[string]string{"main.go": "package main\n\nfunc main() {}\n", "used.go": fmt.Sprintf(uses, "main"), excluded: fmt.Sprintf(uses, "plat")}, true, true},
		"complete":      {map[string]string{"main.go": "package main\n\nfunc main() {}\n", "used.go": fmt.Sprintf(uses, "main")}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			app := codemodApp(t, tc.files)
			needed, err := a.Check(app, upgrade.Baseline{})
			if err != nil || !needed {
				t.Fatalf("Check = %v, %v; want needed", needed, err)
			}
			changed, err := a.Apply(app)
			if changed != tc.changed {
				t.Errorf("Apply changed = %v, want %v", changed, tc.changed)
			}
			if tc.incomplete {
				if !errors.Is(err, upgrade.ErrCodemodIncomplete) || !strings.Contains(err.Error(), excluded) {
					t.Fatalf("Apply = %v; want ErrCodemodIncomplete naming %s", err, excluded)
				}
			} else if err != nil {
				t.Fatalf("Apply = %v", err)
			}
			if tc.changed && !strings.Contains(read(t, filepath.Join(app, "used.go")), "db.Limit") {
				t.Fatal("Apply did not write the type-checked use")
			}
			if _, has := tc.files[excluded]; has && !strings.Contains(read(t, filepath.Join(app, filepath.FromSlash(excluded))), "db.Max") {
				t.Fatal("Apply rewrote a file it did not type-check")
			}
		})
	}
	// A use left for the developer is incomplete too.
	app := codemodApp(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n", "gen.go": "// Code generated by test. DO NOT EDIT.\n\n" + fmt.Sprintf(uses, "main")})
	if _, err := a.Apply(app); !errors.Is(err, upgrade.ErrCodemodIncomplete) || !strings.Contains(err.Error(), "1 use(s) left") {
		t.Fatalf("Apply = %v; want incomplete (left use)", err)
	}
}

// TestRunCodemodsRefusesAPromotedTarget: a member target must be declared
// by the type its Symbol names. Wrapper.Hello is Base's Hello, promoted:
// matched by object, it would rewrite every use of Base.Hello, so the
// codemod is refused, before any Rewrite runs, naming the declaring type.
// The same for a promoted field and an embedded interface's method; the
// declared member itself is a fine target.
func TestRunCodemodsRefusesAPromotedTarget(t *testing.T) {
	const src = `package main

import "github.com/gombit-dev/gombit/db"

func main() {
	db.Base{}.Hello()
	db.Wrapper{}.Hello()
	var g db.Loud
	_ = g
	_ = db.Wide{}.New
}
`
	app := codemodApp(t, map[string]string{"main.go": src})
	var seen []string
	probe := func(symbol string) upgrade.Codemod {
		return upgrade.Codemod{ID: "probe", Description: "records uses", Package: dbPath, Symbol: symbol,
			Rewrite: func(u *upgrade.Use) ast.Expr {
				seen = append(seen, symbol)
				return u.Skip("probe")
			}}
	}
	for symbol, declaredBy := range map[string]string{
		"Wrapper.Hello": dbPath + ".Base",
		"Wide.New":      dbPath + ".Inner",
		"Loud.Hello":    dbPath + ".Greeter",
	} {
		_, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{probe(symbol)})
		if err == nil || !strings.Contains(err.Error(), "promoted from "+declaredBy) {
			t.Errorf("%s: RunCodemods = %v; want it refused as promoted from %s", symbol, err, declaredBy)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("a refused codemod's Rewrite ran: %v", seen)
	}
	// Base.Hello is Base's own: both calls are its uses (one promoted).
	fix, err := upgrade.RunCodemods(context.Background(), app, []upgrade.Codemod{probe("Base.Hello")})
	if err != nil || len(seen) != 2 || len(fix.Left) != 2 {
		t.Fatalf("Base.Hello: %v, seen %v, left %+v", err, seen, fix)
	}
	if read(t, filepath.Join(app, "main.go")) != src {
		t.Fatal("main.go changed")
	}
}
