package upgrade_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// deprecatedReplacement is how a "Deprecated:" paragraph names what to use
// instead.
// instead: "use" followed by what to use (an exported name, possibly
// package-qualified, or a backquoted one), or "instead" / "replaced by".
// "do not use" names nothing.
var deprecatedReplacement = regexp.MustCompile("(?i:\\buse\\s+(the\\s+)?)(`[^`]+`|([a-z][a-z0-9]*\\.)?[A-Z][A-Za-z0-9_]*(\\.[A-Za-z0-9_]+)*)|(?i:\\binstead\\b|\\breplaced by\\b)")

// TestFrameworkDeprecationsNameAReplacement: every "Deprecated:" paragraph
// in the framework says what to use instead, which gombit upgrade check
// shows next to each use.
func TestFrameworkDeprecationsNameAReplacement(t *testing.T) {
	root := filepath.Join("..")
	skip := map[string]bool{"examples": true, "benchmarks": true, "node_modules": true, "testdata": true, ".git": true}
	paragraph := regexp.MustCompile(`(?m)^\s*// Deprecated:(.*(?:\n\s*//[^\n]*\S[^\n]*)*)`)
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		data, err := os.ReadFile(path) // #nosec G304 G122 -- the repository's own sources
		if err != nil {
			return err
		}
		for _, m := range paragraph.FindAllStringSubmatch(string(data), -1) {
			found++
			if !deprecatedReplacement.MatchString(m[1]) {
				t.Errorf("%s: a Deprecated: paragraph names no replacement (say what to use instead): %q", path, strings.TrimSpace(m[0]))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no Deprecated: paragraph at all; the walk is broken")
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
		t.Fatalf("uses = %+v; want database.KeepStoredZeros with its replacement", uses)
	}
}

// TestDeprecatedReplacementRule: the rule the framework's Deprecated:
// paragraphs are held to.
func TestDeprecatedReplacementRule(t *testing.T) {
	for text, want := range map[string]bool{
		"Deprecated: use ScopeEdit with StoredValues.": true,
		"Deprecated: use `ScopeEdit` instead.":         true,
		"Deprecated: use db.Open.":                     true,
		"Deprecated: use this one.":                    false,
		"Deprecated: use the Fetch method.":            true,
		"Deprecated: replaced by Fetch.":               true,
		"Deprecated: call Fetch instead.":              true,
		"Deprecated: do not use.":                      false,
		"Deprecated: do not use this.":                 false,
		"Deprecated: it is going away.":                false,
	} {
		if got := deprecatedReplacement.MatchString(text); got != want {
			t.Errorf("%q: names a replacement = %v, want %v", text, got, want)
		}
	}
}

// TestScanDeprecatedGroupedDeclarationsAndTrimpath: a deprecated type or
// var in a parenthesized group is found, and so is every use when GOFLAGS
// has -trimpath (which would otherwise leave the declarations unreadable).
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
