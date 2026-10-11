package upgrade

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
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
	// Symbol is the API used: pkg.Name, pkg.Type.Field, or (*pkg.Type).Method.
	Symbol string `json:"symbol"`
	// Package is the import path of the package that declares it.
	Package string `json:"package"`
	// Deprecation is the declaration's "Deprecated:" paragraph, which names
	// what to use instead.
	Deprecation string `json:"deprecation"`
}

// ErrDoesNotLoad: the application's packages do not load or type-check, so
// a scan would miss uses; fix the errors first.
var ErrDoesNotLoad = errors.New("upgrade: the application does not type-check")

// ScanDeprecated finds every use, in the application in workDir (all its
// packages, tests included), of a framework API whose declaration carries
// a "Deprecated:" paragraph, the Go convention (go doc, gopls and
// pkg.go.dev show it too). It works on type information, not text: each
// identifier is resolved to the object it denotes (go/packages, go/types),
// so a name that merely matches, a local declaration of the same name, a
// comment or a string never counts, and a method, a field, a composite
// literal key or an embedded field does. The framework's own packages are
// not scanned. An application that does not load or type-check is an
// error (ErrDoesNotLoad), with the go command's diagnostics.
func ScanDeprecated(ctx context.Context, workDir string) ([]DeprecatedUse, error) {
	// Export data names a dependency's files as the build did: under
	// -trimpath (in GOFLAGS, say, as CI often sets it) not as paths on disk,
	// so no declaration could be read. Load without it; GOFLAGS set in the
	// environment wins over go env -w settings, so take the effective value.
	goflags, err := goEnv(ctx, workDir, "GOFLAGS")
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Context: ctx,
		Dir:     workDir,
		Env:     append(os.Environ(), "GOFLAGS="+strings.TrimSpace(goflags+" -trimpath=false")),
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("upgrade: load the application's packages: %w", err)
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
	root, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	docs := newDeclDocs()
	seen := map[string]bool{}
	var uses []DeprecatedUse
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Path == FrameworkModulePath {
			continue // the framework's own packages
		}
		for id, obj := range p.TypesInfo.Uses {
			obj = origin(obj)
			if !isFrameworkObject(obj) {
				continue
			}
			decl, ok, err := docs.deprecation(p.Fset, obj)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			pos := p.Fset.Position(id.Pos())
			key := fmt.Sprintf("%s:%d:%d", pos.Filename, pos.Line, pos.Column)
			if seen[key] { // a package and its test variant share files
				continue
			}
			seen[key] = true
			file := pos.Filename
			if rel, err := filepath.Rel(root, file); err == nil && !strings.HasPrefix(rel, "..") {
				file = filepath.ToSlash(rel)
			}
			uses = append(uses, DeprecatedUse{
				File: file, Line: pos.Line, Column: pos.Column,
				Symbol: decl.symbol, Package: obj.Pkg().Path(), Deprecation: decl.deprecation,
			})
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		a, b := uses[i], uses[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return uses, nil
}

// origin is the generic declaration an instantiated method or field comes
// from, which carries the declaration's position and doc.
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

// isFrameworkObject reports whether obj is declared by a package of the
// framework module.
func isFrameworkObject(obj types.Object) bool {
	if obj == nil || obj.Pkg() == nil || !obj.Pos().IsValid() {
		return false
	}
	path := obj.Pkg().Path()
	return path == FrameworkModulePath || strings.HasPrefix(path, FrameworkModulePath+"/")
}

// declDoc is what a declaration says about itself: the symbol it declares
// and its "Deprecated:" paragraph.
type declDoc struct {
	symbol      string
	deprecation string
}

// declDocs reads the declarations of used objects from their source files,
// parsing each file once: the type information of a dependency carries its
// positions, not its comments.
type declDocs struct {
	fset  *token.FileSet
	files map[string]*ast.File
	// decls are keyed by "file:line:name" of the declared name: a
	// dependency's type information, from export data, places an object at
	// its declaration's line but not always at the name's column.
	decls map[string]declDoc
}

func newDeclDocs() *declDocs {
	return &declDocs{fset: token.NewFileSet(), files: map[string]*ast.File{}, decls: map[string]declDoc{}}
}

// deprecation returns the declaration of obj when it is deprecated. A
// declaring file that cannot be read is an error, never "not deprecated":
// a scan that cannot see a declaration must not report it clean.
func (d *declDocs) deprecation(fset *token.FileSet, obj types.Object) (declDoc, bool, error) {
	pos := fset.Position(obj.Pos())
	if pos.Filename == "" {
		return declDoc{}, false, nil
	}
	if _, parsed := d.files[pos.Filename]; !parsed {
		f, err := parser.ParseFile(d.fset, pos.Filename, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return declDoc{}, false, fmt.Errorf("upgrade: read the declaration of %s.%s: %w", obj.Pkg().Path(), obj.Name(), err)
		}
		d.files[pos.Filename] = f
		d.index(pos.Filename, f, obj.Pkg().Name())
	}
	decl, ok := d.decls[fmt.Sprintf("%s:%d:%s", pos.Filename, pos.Line, obj.Name())]
	return decl, ok && decl.deprecation != "", nil
}

// index records every deprecated declaration of f, by the position of its
// name: functions and methods, types, constants and variables, struct
// fields and interface methods.
func (d *declDocs) index(filename string, f *ast.File, pkg string) {
	add := func(name *ast.Ident, symbol string, docs ...*ast.CommentGroup) {
		for _, doc := range docs {
			if dep := deprecatedParagraph(doc); dep != "" {
				pos := d.fset.Position(name.Pos())
				d.decls[fmt.Sprintf("%s:%d:%s", filename, pos.Line, name.Name)] = declDoc{symbol: symbol, deprecation: dep}
				return
			}
		}
	}
	for _, decl := range f.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			add(decl.Name, funcSymbol(pkg, decl), decl.Doc)
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				// The declaration's own doc applies to a spec that has
				// none, as go doc shows it for an unparenthesized one.
				outer := decl.Doc
				if decl.Lparen.IsValid() {
					outer = nil
				}
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					add(spec.Name, pkg+"."+spec.Name.Name, spec.Doc, outer)
					d.indexMembers(filename, pkg+"."+spec.Name.Name, spec.Type)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						add(name, pkg+"."+name.Name, spec.Doc, outer)
					}
				}
			}
		}
	}
}

// indexMembers records the deprecated fields of a struct type, and the
// deprecated methods of an interface type, named owner.
func (d *declDocs) indexMembers(filename, owner string, typ ast.Expr) {
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
		dep := deprecatedParagraph(field.Doc)
		if dep == "" {
			continue
		}
		for _, name := range field.Names {
			pos := d.fset.Position(name.Pos())
			d.decls[fmt.Sprintf("%s:%d:%s", filename, pos.Line, name.Name)] = declDoc{symbol: owner + "." + name.Name, deprecation: dep}
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
	// A generic receiver: T[P].
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	name := pkg + "."
	if id, ok := recv.(*ast.Ident); ok {
		name += id.Name
	}
	if star {
		return "(*" + name + ")." + fn.Name.Name
	}
	return name + "." + fn.Name.Name
}

// deprecatedParagraph is doc's "Deprecated:" paragraph, the Go convention
// (a paragraph of the doc comment that starts with it), on one line; empty
// when there is none.
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
