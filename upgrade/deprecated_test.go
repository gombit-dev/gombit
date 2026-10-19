package upgrade_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/upgrade"
)

// writeTree writes files (path -> content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G703 -- under t.TempDir()
			t.Fatal(err)
		}
	}
}

// fakeFramework is a module at the framework's path, with deprecated APIs
// in every place a declaration can carry a "Deprecated:" paragraph.
var fakeFramework = map[string]string{
	"go.mod": "module github.com/gombit-dev/gombit\n\ngo 1.21\n",
	"db/db.go": `package db

// Old opens a store.
//
// Deprecated: use Open, which also takes a context.
func Old() *Store { return nil }

// Open opens a store.
func Open() *Store { return nil }

// Store is a store.
type Store struct {
	// Name names it.
	//
	// Deprecated: use Label.
	Name  string
	Label string
}

// Get gets.
//
// Deprecated: use Fetch instead.
func (s *Store) Get() {}

// Fetch fetches.
func (s *Store) Fetch() {}

// Legacy is a legacy store.
//
// Deprecated: use Store.
type Legacy struct{}

const (
	// Max is the old limit.
	//
	// Deprecated: use Limit.
	Max   = 10
	Limit = 10
)

// Getter gets.
type Getter interface {
	// Get gets.
	//
	// Deprecated: use Fetch.
	Get()
	Fetch()
}

// Fresh opens a store. Its doc mentions the word, mid-paragraph, as
// "Deprecated" Old's replacement: that is not a deprecation.
func Fresh() *Store { return nil }

// Box is a generic box.
type Box[T any] struct{ v T }

// Peek peeks.
//
// Deprecated: use Value.
func (b Box[T]) Peek() T { return b.v }

// Dated opens a store.
//
// Deprecated: since v0.8.0, use Open.
func Dated() *Store { return nil }

// Wrapped declares its struct and interface in parentheses.
type Wrapped (struct {
	// Tag tags.
	//
	// Deprecated: use Label.
	Tag string
})

// Pinger pings.
type Pinger (interface {
	// Ping pings.
	//
	// Deprecated: use Fetch.
	Ping()
})
`,
}

const deprecatedApp = `package main

import (
	"example.com/other"
	"github.com/gombit-dev/gombit/db"
)

// A local name shadowing a deprecated one is not a use of it:
func Old() {}

func main() {
	Old() // "db.Old" in a comment or a string is not a use either
	_ = "db.Old()"
	s := db.Old()
	s.Get()
	_ = s.Name
	_ = db.Store{Name: "x"}
	_ = db.Legacy{}
	_ = db.Max
	var g db.Getter = s
	g.Get()
	_ = db.Box[int]{}.Peek()
	s.Fetch()
	_ = db.Open()
	_ = db.Limit
	_ = db.Fresh()
	other.Gone() // deprecated, but not a framework API
}
`

func scanApp(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	writeTree(t, filepath.Join(root, "fw"), fakeFramework)
	writeTree(t, filepath.Join(root, "other"), map[string]string{
		"go.mod":   "module example.com/other\n\ngo 1.21\n",
		"other.go": "package other\n\n// Gone is gone.\n//\n// Deprecated: use nothing.\nfunc Gone() {}\n",
	})
	app := filepath.Join(root, "app")
	all := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.21\n\nrequire (\n\tgithub.com/gombit-dev/gombit v0.0.0\n\texample.com/other v0.0.0\n)\n\nreplace github.com/gombit-dev/gombit => ../fw\n\nreplace example.com/other => ../other\n",
	}
	for k, v := range files {
		all[k] = v
	}
	writeTree(t, app, all)
	return app
}

// TestScanDeprecated: every use of a deprecated framework API is reported
// with its file, position, symbol and what to use instead, resolved from
// type information: a shadowing local name, a comment or a string is not a
// use, and a non-deprecated API is not reported.
func TestScanDeprecated(t *testing.T) {
	app := scanApp(t, map[string]string{
		"main.go": deprecatedApp,
		"main_test.go": `package main

import (
	"testing"

	"github.com/gombit-dev/gombit/db"
)

func TestUse(t *testing.T) { _ = db.Old() }
`,
	})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range uses {
		got = append(got, u.File+" "+u.Symbol)
		if u.Package != "github.com/gombit-dev/gombit/db" || !strings.HasPrefix(u.Deprecation, "Deprecated: use ") || u.Line == 0 || u.Column == 0 {
			t.Errorf("use %+v", u)
		}
	}
	want := []string{
		"main.go db.Old",
		"main.go (*db.Store).Get",
		"main.go db.Store.Name",
		"main.go db.Store.Name", // the composite literal key
		"main.go db.Legacy",
		"main.go db.Max",
		"main.go db.Getter.Get",
		"main.go db.Box.Peek",
		"main_test.go db.Old",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("uses:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if uses[0].Line != 14 || uses[0].Deprecation != "Deprecated: use Open, which also takes a context." {
		t.Fatalf("first use = %+v; want db.Old at line 11 with its paragraph", uses[0])
	}

	var out strings.Builder
	if err := upgrade.RenderDeprecated(&out, uses); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Deprecated framework APIs used: 9\n  main.go:14:") || !strings.Contains(out.String(), "      Deprecated: use Open, which also takes a context.\n") {
		t.Fatalf("Render:\n%s", out.String())
	}
}

// TestScanDeprecatedNone: an app using no deprecated API gets none.
func TestScanDeprecatedNone(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { db.Open().Fetch() }\n"})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil || len(uses) != 0 {
		t.Fatalf("ScanDeprecated = %+v, %v; want none", uses, err)
	}
	var out strings.Builder
	if err := upgrade.RenderDeprecated(&out, uses); err != nil || out.String() != "No deprecated framework APIs used.\n" {
		t.Fatalf("Render = %q, %v", out.String(), err)
	}
}

// TestScanDeprecatedRefusesAnAppThatDoesNotTypeCheck: a scan of code that
// does not type-check would miss uses, so it is an error, with the
// diagnostics.
func TestScanDeprecatedRefusesAnAppThatDoesNotTypeCheck(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/db\"\n\nfunc main() { db.Old().Nope() }\n"})
	_, err := upgrade.ScanDeprecated(context.Background(), app)
	if !errors.Is(err, upgrade.ErrDoesNotLoad) || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("ScanDeprecated = %v; want ErrDoesNotLoad naming the type error", err)
	}
}

// TestScanDeprecatedRealFramework: against this repository's framework
// (database.KeepStoredZeros is deprecated), with the module cache only.
func TestScanDeprecatedRealFramework(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(repo, "go.sum")) // #nosec G304 -- the repository's go.sum
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	app := t.TempDir()
	writeTree(t, app, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.26\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => " + filepath.ToSlash(repo) + "\n",
		"go.sum": string(sum),
		"main.go": `package main

import "github.com/gombit-dev/gombit/database"

func main() { _ = database.KeepStoredZeros(nil, nil, nil, nil) }
`,
	})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil && (strings.Contains(err.Error(), "module lookup disabled") || strings.Contains(err.Error(), "missing go.sum entry")) {
		t.Skipf("the framework's dependencies are not all in the module cache (offline): %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].Symbol != "database.KeepStoredZeros" || !strings.Contains(uses[0].Deprecation, "use ScopeEdit with StoredValues") {
		t.Fatalf("uses = %+v; want database.KeepStoredZeros with its replacement\n%s", uses, goDiagnostics(app))
	}
}

// TestScanDeprecatedGroupedDeclarationsAndTrimpath: a deprecated type or
// var in a parenthesized group is found, and GOFLAGS=-trimpath changes
// nothing: the scan type-checks from source and reads no file name a build
// records.
func TestScanDeprecatedGroupedDeclarationsAndTrimpath(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": "package main\n\nimport \"github.com/gombit-dev/gombit/grp\"\n\nfunc main() { _ = grp.Old{}; _ = grp.V; _ = grp.Fine{} }\n"})
	writeTree(t, filepath.Join(app, "..", "fw"), map[string]string{"grp/grp.go": "package grp\n\ntype (\n\t// Fine is fine.\n\tFine struct{}\n\n\t// Old is old.\n\t//\n\t// Deprecated: use Fine.\n\tOld struct{}\n)\n\nvar (\n\t// V is v.\n\t//\n\t// Deprecated: use W.\n\tV = 1\n\tW = 2\n)\n"})
	for _, flags := range []string{"-mod=mod", "-mod=mod -trimpath"} {
		t.Setenv("GOFLAGS", flags)
		uses, err := upgrade.ScanDeprecated(context.Background(), app)
		if err != nil || len(uses) != 2 || uses[0].Symbol != "grp.Old" || uses[1].Symbol != "grp.V" {
			t.Fatalf("GOFLAGS=%q: uses = %+v, %v; want grp.Old and grp.V", flags, uses, err)
		}
	}
}

// TestScanDeprecatedBeyondIdentifiers: uses that no identifier of the
// deprecated declaration spells are found too: the import of a deprecated
// package, an element of an unkeyed struct literal, and a deprecated
// embedded field (whose name is its type's); and a deprecation in a block
// doc comment counts like a line one.
func TestScanDeprecatedBeyondIdentifiers(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": `package main

import (
	"github.com/gombit-dev/gombit/legacy"
	"github.com/gombit-dev/gombit/opts"
)

func main() {
	legacy.Run()
	_ = opts.Options{"x", 0}
	c := opts.Config{Base: opts.Base{}, Ptr: nil}
	_ = c.Base
	_ = c.Ptr
	_ = opts.Blocky()
}
`})
	writeTree(t, filepath.Join(app, "..", "fw"), map[string]string{
		"legacy/legacy.go": "// Package legacy runs things.\n//\n// Deprecated: use package opts instead.\npackage legacy\n\n// Run runs.\nfunc Run() {}\n",
		"opts/opts.go": `package opts

// Options are options.
type Options struct {
	// Name names.
	//
	// Deprecated: use Mode.
	Name string
	Mode int
}

// Base is a base.
type Base struct{}

// Ptr is a pointed base.
type Ptr struct{}

// Config embeds them.
type Config struct {
	// Deprecated: use Options.
	Base
	// Deprecated: use Options instead.
	*Ptr
}

/*
Blocky is documented in a block comment.

Deprecated: use Options instead.
*/
func Blocky() int { return 0 }
`,
	})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range uses {
		got = append(got, fmt.Sprintf("%d %s", u.Line, u.Symbol))
	}
	want := []string{
		"4 github.com/gombit-dev/gombit/legacy", // the import
		"10 opts.Options.Name",                  // the first unkeyed element
		"11 opts.Config.Base",                   // the keyed embedded field
		"11 opts.Config.Ptr",
		"12 opts.Config.Base", // its selector
		"13 opts.Config.Ptr",
		"14 opts.Blocky",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("uses:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if uses[0].Deprecation != "Deprecated: use package opts instead." {
		t.Fatalf("package deprecation = %q", uses[0].Deprecation)
	}
}

// goDiagnostics is what the go command sees in dir: its environment and
// the framework packages in the build, for a failure message.
func goDiagnostics(dir string) string {
	var b strings.Builder
	for _, args := range [][]string{
		{"env", "GOFLAGS", "GOWORK", "GOPROXY", "GOMODCACHE", "GOVERSION"},
		{"list", "-deps", "-f", "{{.ImportPath}} {{.Dir}} {{.Error}}", "./..."},
	} {
		cmd := exec.Command("go", args...) // #nosec G204 -- fixed go subcommands, for diagnostics
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		fmt.Fprintf(&b, "$ go %s (%v)\n", strings.Join(args, " "), err)
		for _, line := range strings.Split(string(out), "\n") {
			if args[0] == "env" || strings.Contains(line, "gombit-dev/gombit") || strings.Contains(line, "example.com") {
				b.WriteString(line + "\n")
			}
		}
	}
	return b.String()
}

// TestScanDeprecatedThroughADependency: a framework API the app reaches
// only through a non-framework package (a wrapper embedding a framework
// type, whose promoted method the app calls) is found, though the app
// imports no framework package.
func TestScanDeprecatedThroughADependency(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": `package main

import "example.com/wrap"

func main() { wrap.New().Get() }
`})
	root := filepath.Join(app, "..")
	writeTree(t, filepath.Join(root, "wrap"), map[string]string{
		"go.mod":  "module example.com/wrap\n\ngo 1.21\n\nrequire github.com/gombit-dev/gombit v0.0.0\n\nreplace github.com/gombit-dev/gombit => ../fw\n",
		"wrap.go": "package wrap\n\nimport \"github.com/gombit-dev/gombit/db\"\n\n// W wraps a store.\ntype W struct{ *db.Store }\n\n// New returns a wrapper.\nfunc New() W { return W{} }\n",
	})
	goMod := filepath.Join(app, "go.mod")
	data, err := os.ReadFile(goMod) // #nosec G304 -- under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, "require example.com/wrap v0.0.0\n\nreplace example.com/wrap => ../wrap\n"...)
	if err := os.WriteFile(goMod, data, 0o600); err != nil { // #nosec G703 -- under t.TempDir()
		t.Fatal(err)
	}
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].Symbol != "(*db.Store).Get" || uses[0].Line != 5 {
		t.Fatalf("uses = %+v; want the promoted (*db.Store).Get", uses)
	}
}

// TestScanDeprecatedTwoUsesAtOneToken: an unkeyed element that is itself a
// deprecated name uses two deprecated declarations at one position, the
// field it fills and the name; both are reported.
func TestScanDeprecatedTwoUsesAtOneToken(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": `package main

import . "github.com/gombit-dev/gombit/db"

func main() { _ = Store{OldName, ""} }
`})
	writeTree(t, filepath.Join(app, "..", "fw"), map[string]string{"db/extra.go": "package db\n\n// OldName is an old name.\n//\n// Deprecated: use Label.\nconst OldName = \"x\"\n"})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range uses {
		got = append(got, fmt.Sprintf("%d:%d %s", u.Line, u.Column, u.Symbol))
	}
	// Store's first field is Name (deprecated); OldName is a deprecated
	// const.
	if strings.Join(got, "|") != "5:25 db.OldName|5:25 db.Store.Name" {
		t.Fatalf("uses = %v; want db.OldName and db.Store.Name at the same token", got)
	}
}

// TestScanDeprecatedParenthesizedDeclarations: a struct or interface
// declared in parentheses (type T (struct{...})) has its members' docs
// read like any other.
func TestScanDeprecatedParenthesizedDeclarations(t *testing.T) {
	app := scanApp(t, map[string]string{"main.go": `package main

import "github.com/gombit-dev/gombit/db"

func main() {
	_ = db.Wrapped{}.Tag
	var p db.Pinger
	p.Ping()
}
`})
	uses, err := upgrade.ScanDeprecated(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range uses {
		got = append(got, u.Symbol)
	}
	if strings.Join(got, ",") != "db.Wrapped.Tag,db.Pinger.Ping" {
		t.Fatalf("uses = %v", got)
	}
}
