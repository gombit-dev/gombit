package upgrade

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/objectpath"
)

// DeprecatedUse is one use, in the application, of a framework API that the
// framework marks deprecated.
type DeprecatedUse struct {
	// File is the using file, relative to the application directory when
	// it is inside it.
	File string `json:"file"`
	// Line and Column are where the use is.
	Line   int `json:"line"`
	Column int `json:"column"`
	// Symbol is the API used: an import path (a deprecated package),
	// pkg.Name, pkg.Type.Field, or (*pkg.Type).Method.
	Symbol string `json:"symbol"`
	// Package is the import path of the package that declares it.
	Package string `json:"package"`
	// Deprecation is the declaration's "Deprecated:" paragraph, which names
	// what to use instead.
	Deprecation string `json:"deprecation"`
}

// ErrDoesNotLoad: the application's packages (or the framework packages
// they import) do not load or type-check, so a scan would miss uses; fix the
// errors first.
var ErrDoesNotLoad = errors.New("upgrade: the application does not type-check")

// loadMode is what a scan needs of a package: its syntax with full type
// information.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
	packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule | packages.NeedImports

// ScanDeprecated finds every use, in the application in workDir (all its
// packages, tests included), of a framework API whose doc comment carries a
// "Deprecated:" paragraph, the Go convention (go doc, gopls and pkg.go.dev
// show it too): a deprecated package (its import), or a deprecated
// function, method, type, constant, variable, struct field (named or
// embedded) or interface method.
//
// It works on type information, never text. The framework packages the app
// imports are loaded from source, and each deprecated declaration is tied
// to the object it defines (go/types); every use in the app is resolved to
// its object, and the two meet by their object path (go/types/objectpath),
// the identity gopls also uses across source and export data, never by file
// positions. A use is an identifier denoting such an object (a call, a
// selector, a composite literal key, a dot-imported name), an element of an
// unkeyed struct literal (which names no field), or the import of a
// deprecated package. So a name that merely matches, a local declaration of
// the same name, a comment or a string never counts. The framework's own
// packages are not scanned. Whatever does not load or type-check, or a
// framework API object whose identity cannot be resolved, is an error,
// never a clean result.
func ScanDeprecated(ctx context.Context, workDir string) ([]DeprecatedUse, error) {
	cfg, err := scanConfig(ctx, workDir)
	if err != nil {
		return nil, err
	}
	pkgs, err := loadPackages(cfg, "./...")
	if err != nil {
		return nil, err
	}
	var apps []*packages.Package
	framework := map[string]bool{}
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Path == FrameworkModulePath {
			continue // the framework's own packages
		}
		apps = append(apps, p)
		for path := range p.Imports {
			if isFrameworkPath(path) {
				framework[path] = true
			}
		}
	}
	if len(framework) == 0 {
		return nil, nil
	}
	deprecated, err := frameworkDeprecations(cfg, framework)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	s := &scan{root: root, deprecated: deprecated, seen: map[string]bool{}}
	for _, p := range apps {
		if err := s.pkg(p); err != nil {
			return nil, err
		}
	}
	sort.Slice(s.uses, func(i, j int) bool {
		a, b := s.uses[i], s.uses[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return s.uses, nil
}

// scanConfig is the load configuration for the app in workDir. Export data
// names a dependency's files as the build did, which -trimpath (in GOFLAGS,
// as CI often sets it) leaves unusable for anything reading them; load
// without it. GOFLAGS set in the environment wins over go env -w settings,
// so the effective value is extended.
func scanConfig(ctx context.Context, workDir string) (*packages.Config, error) {
	goflags, err := goEnv(ctx, workDir, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	return &packages.Config{
		Context: ctx,
		Dir:     workDir,
		Env:     append(os.Environ(), "GOFLAGS="+strings.TrimSpace(goflags+" -trimpath=false")),
		Mode:    loadMode,
		Tests:   true,
	}, nil
}

// loadPackages loads patterns and refuses any package that does not load or
// type-check.
func loadPackages(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("upgrade: load packages: %w", err)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	if len(loadErrs) > 0 {
		if len(loadErrs) > 10 {
			loadErrs = append(loadErrs[:10], fmt.Sprintf("... and %d more", len(loadErrs)-10))
		}
		return nil, fmt.Errorf("%w:\n%s", ErrDoesNotLoad, strings.Join(loadErrs, "\n"))
	}
	return pkgs, nil
}

// declDoc is a deprecated declaration: the symbol it declares and its
// "Deprecated:" paragraph.
type declDoc struct {
	symbol      string
	deprecation string
}

// frameworkDeprecations loads the framework packages paths from source, the
// way the app builds them (cfg), and returns their deprecated declarations
// by object key (objectKey), and a deprecated package by its path alone.
func frameworkDeprecations(cfg *packages.Config, paths map[string]bool) (map[string]declDoc, error) {
	fw := *cfg
	fw.Tests = false
	patterns := make([]string, 0, len(paths))
	for p := range paths {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	pkgs, err := loadPackages(&fw, patterns...)
	if err != nil {
		return nil, err
	}
	out := map[string]declDoc{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			var walkErr error
			walkDocs(p.Name, f, func(name *ast.Ident, symbol string, docs []*ast.CommentGroup) {
				dep := firstDeprecation(docs)
				if dep == "" || walkErr != nil {
					return
				}
				if name == nil { // the package clause
					out[p.PkgPath] = declDoc{symbol: p.PkgPath, deprecation: dep}
					return
				}
				obj := p.TypesInfo.Defs[name]
				if obj == nil {
					walkErr = fmt.Errorf("upgrade: %s: no object for the deprecated %s", p.PkgPath, symbol)
					return
				}
				key, err := objectKey(obj)
				if err != nil {
					walkErr = err
					return
				}
				out[key] = declDoc{symbol: symbol, deprecation: dep}
			})
			if walkErr != nil {
				return nil, walkErr
			}
		}
	}
	return out, nil
}

// objectKey is obj's identity across source and export data: its package
// path and object path.
func objectKey(obj types.Object) (string, error) {
	path, err := objectpath.For(obj)
	if err != nil {
		return "", fmt.Errorf("upgrade: resolve %s.%s: %w", obj.Pkg().Path(), obj.Name(), err)
	}
	return obj.Pkg().Path() + "\x00" + string(path), nil
}

// scan collects the uses in the app's packages.
type scan struct {
	root       string
	deprecated map[string]declDoc
	seen       map[string]bool // positions already reported
	uses       []DeprecatedUse
}

func (s *scan) pkg(p *packages.Package) error {
	for id, obj := range p.TypesInfo.Uses {
		if err := s.object(p, id.Pos(), obj); err != nil {
			return err
		}
	}
	for _, f := range p.Syntax {
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if d, ok := s.deprecated[path]; ok {
				s.add(p, spec.Path.Pos(), path, d)
			}
		}
		var err error
		ast.Inspect(f, func(n ast.Node) bool {
			if err != nil {
				return false
			}
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			st, ok := structOf(p.TypesInfo, lit)
			if !ok {
				return true
			}
			for i, el := range lit.Elts {
				if _, keyed := el.(*ast.KeyValueExpr); keyed || i >= st.NumFields() {
					continue
				}
				// An unkeyed element names no field: it is the i-th.
				if err = s.object(p, el.Pos(), st.Field(i)); err != nil {
					return false
				}
			}
			return true
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// structOf is the struct type a composite literal builds, if it does.
func structOf(info *types.Info, lit *ast.CompositeLit) (*types.Struct, bool) {
	t := info.TypeOf(lit)
	if t == nil {
		return nil, false
	}
	st, ok := t.Underlying().(*types.Struct)
	return st, ok
}

// object records a use of obj at pos when obj is a deprecated framework
// declaration. Every object of the framework's API has an object path; one
// without (a framework-local declaration) is not API, and cannot be used
// from the app.
func (s *scan) object(p *packages.Package, pos token.Pos, obj types.Object) error {
	obj = origin(obj)
	if !isFrameworkObject(obj) {
		return nil
	}
	if _, isPkg := obj.(*types.PkgName); isPkg {
		return nil // a package name: its import is checked instead
	}
	key, err := objectKey(obj)
	if err != nil {
		return err
	}
	if d, ok := s.deprecated[key]; ok {
		s.add(p, pos, obj.Pkg().Path(), d)
	}
	return nil
}

func (s *scan) add(p *packages.Package, pos token.Pos, pkgPath string, d declDoc) {
	at := p.Fset.Position(pos)
	key := fmt.Sprintf("%s:%d:%d", at.Filename, at.Line, at.Column)
	if s.seen[key] { // a package and its test variant share files
		return
	}
	s.seen[key] = true
	file := at.Filename
	if rel, err := filepath.Rel(s.root, file); err == nil && !strings.HasPrefix(rel, "..") {
		file = filepath.ToSlash(rel)
	}
	s.uses = append(s.uses, DeprecatedUse{
		File: file, Line: at.Line, Column: at.Column,
		Symbol: d.symbol, Package: pkgPath, Deprecation: d.deprecation,
	})
}

// origin is the generic declaration an instantiated method or field comes
// from.
func origin(obj types.Object) types.Object {
	switch o := obj.(type) {
	case *types.Func:
		return o.Origin()
	case *types.Var:
		return o.Origin()
	}
	return obj
}

// goEnv is the go command's value of the variable name in dir.
func goEnv(ctx context.Context, dir, name string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", name) // #nosec G204 -- go env with a fixed variable name
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("upgrade: go env %s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func isFrameworkPath(path string) bool {
	return path == FrameworkModulePath || strings.HasPrefix(path, FrameworkModulePath+"/")
}

// isFrameworkObject reports whether obj is declared by a package of the
// framework module.
func isFrameworkObject(obj types.Object) bool {
	return obj != nil && obj.Pkg() != nil && isFrameworkPath(obj.Pkg().Path())
}

// walkDocs calls fn for every declaration of f that can carry a doc
// comment, with the identifier it declares (nil for the package clause),
// the symbol it names in pkg, and its doc comments, innermost first: the
// package clause, functions and methods, types, constants and variables
// (a spec's own doc, then an unparenthesized declaration's), struct fields
// (named, and embedded: their name is their type's, and go/types records
// the field at that identifier) and interface methods. The scan and the
// framework's doc rule both read docs through it.
func walkDocs(pkg string, f *ast.File, fn func(name *ast.Ident, symbol string, docs []*ast.CommentGroup)) {
	fn(nil, pkg, []*ast.CommentGroup{f.Doc})
	for _, decl := range f.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			fn(decl.Name, funcSymbol(pkg, decl), []*ast.CommentGroup{decl.Doc})
		case *ast.GenDecl:
			// The declaration's own doc applies to a spec that has none, as
			// go doc shows it for an unparenthesized one.
			outer := decl.Doc
			if decl.Lparen.IsValid() {
				outer = nil
			}
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					fn(spec.Name, pkg+"."+spec.Name.Name, []*ast.CommentGroup{spec.Doc, outer})
					walkMembers(pkg+"."+spec.Name.Name, spec.Type, fn)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						fn(name, pkg+"."+name.Name, []*ast.CommentGroup{spec.Doc, outer})
					}
				}
			}
		}
	}
}

// walkMembers calls fn for the fields of a struct type, or the methods of
// an interface type, named owner.
func walkMembers(owner string, typ ast.Expr, fn func(name *ast.Ident, symbol string, docs []*ast.CommentGroup)) {
	var fields *ast.FieldList
	switch t := typ.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	default:
		return
	}
	for _, field := range fields.List {
		docs := []*ast.CommentGroup{field.Doc}
		if len(field.Names) == 0 {
			if _, isInterface := typ.(*ast.InterfaceType); isInterface {
				continue // an embedded interface declares no member of its own
			}
			// An embedded field: its name is its type's.
			if name := embeddedName(field.Type); name != nil {
				fn(name, owner+"."+name.Name, docs)
			}
			continue
		}
		for _, name := range field.Names {
			fn(name, owner+"."+name.Name, docs)
		}
	}
}

// embeddedName is the identifier naming an embedded field of type typ:
// T, *T, pkg.T, T[P] and their combinations.
func embeddedName(typ ast.Expr) *ast.Ident {
	for {
		switch t := typ.(type) {
		case *ast.Ident:
			return t
		case *ast.StarExpr:
			typ = t.X
		case *ast.SelectorExpr:
			return t.Sel
		case *ast.IndexExpr:
			typ = t.X
		case *ast.IndexListExpr:
			typ = t.X
		case *ast.ParenExpr:
			typ = t.X
		default:
			return nil
		}
	}
}

// funcSymbol names a function pkg.F, or a method (*pkg.T).M or pkg.T.M.
func funcSymbol(pkg string, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return pkg + "." + fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	star := false
	if s, ok := recv.(*ast.StarExpr); ok {
		star, recv = true, s.X
	}
	name := pkg + "."
	if id := embeddedName(recv); id != nil {
		name += id.Name
	}
	if star {
		return "(*" + name + ")." + fn.Name.Name
	}
	return name + "." + fn.Name.Name
}

// firstDeprecation is the first "Deprecated:" paragraph among docs.
func firstDeprecation(docs []*ast.CommentGroup) string {
	for _, doc := range docs {
		if dep := deprecatedParagraph(doc); dep != "" {
			return dep
		}
	}
	return ""
}

// deprecatedParagraph is doc's "Deprecated:" paragraph, the Go convention
// (a paragraph of the doc comment that starts with it), on one line; empty
// when there is none. Line and block comments read the same (Text).
func deprecatedParagraph(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	for _, para := range strings.Split(doc.Text(), "\n\n") {
		if para = strings.TrimSpace(para); strings.HasPrefix(para, "Deprecated:") {
			return strings.Join(strings.Fields(para), " ")
		}
	}
	return ""
}

// RenderDeprecated writes the uses, each with what to use instead.
func RenderDeprecated(w io.Writer, uses []DeprecatedUse) error {
	var b strings.Builder
	if len(uses) == 0 {
		b.WriteString("No deprecated framework APIs used.\n")
	} else {
		fmt.Fprintf(&b, "Deprecated framework APIs used: %d\n", len(uses))
		for _, u := range uses {
			fmt.Fprintf(&b, "  %s:%d:%d: %s\n      %s\n", u.File, u.Line, u.Column, u.Symbol, u.Deprecation)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
