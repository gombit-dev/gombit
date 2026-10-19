package upgrade

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// deprecatedReplacement is how a "Deprecated:" paragraph names what to use
// instead: "use" followed by what to use (an exported name, possibly
// package-qualified, or a backquoted one), or "instead" / "replaced by".
// "do not use" names nothing.
var deprecatedReplacement = regexp.MustCompile("(?i:\\buse\\s+(the\\s+)?)(`[^`]+`|([a-z][a-z0-9]*\\.)?[A-Z][A-Za-z0-9_]*(\\.[A-Za-z0-9_]+)*)|(?i:\\binstead\\b|\\breplaced by\\b)")

// deprecationsOf is every "Deprecated:" paragraph of f's documentation, as
// the scan reads it (walkDocs, deprecatedParagraph): doc comments of the
// package, declarations and members, line or block, never a comment inside
// a function body.
func deprecationsOf(f *ast.File) []string {
	var out []string
	walkDocs(f.Name.Name, f, func(_ *ast.Ident, symbol string, docs []*ast.CommentGroup) {
		for _, doc := range docs {
			if dep := deprecatedParagraph(doc); dep != "" {
				out = append(out, symbol+": "+dep)
			}
		}
	})
	return out
}

// TestFrameworkDeprecationsNameAReplacement: every "Deprecated:" paragraph
// in the framework's documentation (parsed, as the scan reads it) says what
// to use instead, which gombit upgrade check shows next to each use.
func TestFrameworkDeprecationsNameAReplacement(t *testing.T) {
	skip := map[string]bool{"examples": true, "benchmarks": true, "node_modules": true, "testdata": true, ".git": true}
	fset := token.NewFileSet()
	found := 0
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
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, dep := range deprecationsOf(f) {
			found++
			if !deprecatedReplacement.MatchString(dep) {
				t.Errorf("%s: a Deprecated: paragraph names no replacement (say what to use instead): %q", path, dep)
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

// TestDeprecationsAreDocs: a deprecation in a block doc comment, on the
// package clause, or on an embedded field counts; a "Deprecated:" comment
// inside a function body is no declaration's documentation and does not.
func TestDeprecationsAreDocs(t *testing.T) {
	src := `/*
Package p is old.

Deprecated: do not use.
*/
package p

/*
F does things.

Deprecated: use G.
*/
func F() {
	// Deprecated: this is a body comment, not documentation.
}

// T has fields.
type T struct {
	// Deprecated: use U.
	Base
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(deprecationsOf(f), "\n")
	want := "p: Deprecated: do not use.\np.F: Deprecated: use G.\np.T.Base: Deprecated: use U."
	if got != want {
		t.Fatalf("deprecations:\n%s\nwant:\n%s", got, want)
	}
	// The block-comment package deprecation names nothing: the rule
	// refuses it.
	if deprecatedReplacement.MatchString("Deprecated: do not use.") {
		t.Fatal("the rule accepted a deprecation naming no replacement")
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

// TestScanFailsClosedOnAnUnindexedPackage: a framework object whose
// package the index never saw cannot be judged, and stops the scan with an
// error; it is never taken for "not deprecated".
func TestScanFailsClosedOnAnUnindexedPackage(t *testing.T) {
	pkg := types.NewPackage(FrameworkModulePath+"/db", "db")
	obj := types.NewFunc(token.NoPos, pkg, "Old", types.NewSignatureType(nil, nil, nil, nil, nil, false))
	s := &scan{idx: &index{objects: map[types.Object]declDoc{}, packages: map[string]declDoc{}, indexed: map[*types.Package]bool{}}, seen: map[string]bool{}}
	if err := s.object(&packages.Package{Fset: token.NewFileSet()}, token.NoPos, obj); err == nil || !strings.Contains(err.Error(), FrameworkModulePath+"/db.Old comes from a framework package outside the loaded graph") {
		t.Fatalf("object = %v; want an error", err)
	}
	if len(s.uses) != 0 {
		t.Fatalf("uses = %+v", s.uses)
	}
}

// TestFrameworkDeprecationsNameTheirRelease: every Deprecated: paragraph in
// the framework names the release that deprecated the API ("Deprecated:
// since vX.Y.Z, ..."), so the compatibility policy's window (RemovableFrom)
// starts somewhere: a release the manifest lists, or the upcoming one. That
// release declares the deprecation (a change whose deprecates names the
// API, in its entry, a pre-release of it, or the unreleased entry for the
// upcoming release), so the window cannot be backdated.
func TestFrameworkDeprecationsNameTheirRelease(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	skip := map[string]bool{"examples": true, "benchmarks": true, "node_modules": true, "testdata": true, ".git": true}
	fset := token.NewFileSet()
	found := 0
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
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
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		upcoming := UpcomingReleases(m.latestStable())
		for _, dep := range deprecationsOf(f) {
			found++
			symbol, _, _ := strings.Cut(dep, ": ")
			since := DeprecationSince(dep)
			if since == "" {
				t.Errorf("%s: a Deprecated: paragraph names no release (\"Deprecated: since vX.Y.Z, use ...\"): %q", path, dep)
				continue
			}
			_, err := m.Release(since)
			if err != nil && !slices.Contains(upcoming, since) {
				t.Errorf("%s: deprecated since %s, which is neither a release the manifest lists nor the next one (%v)", path, since, upcoming)
				continue
			}
			// The release, its pre-releases, and (not cut yet) unreleased.
			declared := slices.ContainsFunc(m.Releases, func(r Release) bool {
				ships := r.Version == since || strings.HasPrefix(r.Version, since+"-") || (err != nil && r.Version == Unreleased)
				return ships && slices.ContainsFunc(r.Changes, func(c Change) bool { return slices.Contains(c.Deprecates, symbol) })
			})
			if !declared {
				t.Errorf("%s: %s is deprecated since %s, but no change of that release declares it (deprecates: [%s] in upgrade/manifest.yaml)", path, symbol, since, symbol)
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
