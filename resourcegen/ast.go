package resourcegen

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	"github.com/gombit-dev/gombit/migrations"
)

// AddImportAndRegister adds `importPath` and `<pkg>.Register(app)` next to an
// existing `*.Register(app)` call (the scaffold's product.Register) or, if
// none exists, immediately before `framework.Run(app)` — including the
// `if err := framework.Run(app); err != nil` form used by the app template.
// The edit is idempotent: a second call does not duplicate the import or the
// Register invocation.
func AddImportAndRegister(src []byte, importPath, pkgName string) ([]byte, error) {
	if strings.TrimSpace(importPath) == "" || !isGoIdent(pkgName) {
		return nil, fmt.Errorf("resourcegen: invalid import %q package %q", importPath, pkgName)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, serverMainRel, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("resourcegen: parse %s: %w", serverMainRel, err)
	}
	addImport(file, importPath)
	if err := insertRegisterCall(file, pkgName); err != nil {
		return nil, err
	}
	return formatFile(fset, file)
}

// AddAutoMigrateModel adds `&<pkg>.<type>{}` to the AutoMigrate call in
// internal/platform and ensures importPath is imported. Idempotent.
func AddAutoMigrateModel(src []byte, importPath, pkgName, typeName string) ([]byte, error) {
	if strings.TrimSpace(importPath) == "" || !isGoIdent(pkgName) || !isExportedIdent(typeName) {
		return nil, fmt.Errorf("resourcegen: invalid AutoMigrate target %s.%s", pkgName, typeName)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, platformDBRel, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("resourcegen: parse %s: %w", platformDBRel, err)
	}
	addImport(file, importPath)
	if err := insertAutoMigrateArg(file, pkgName, typeName); err != nil {
		return nil, err
	}
	return formatFile(fset, file)
}

func formatFile(fset *token.FileSet, file *ast.File) ([]byte, error) {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, fmt.Errorf("resourcegen: format: %w", err)
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("resourcegen: format source: %w", err)
	}
	return formatted, nil
}

func addImport(file *ast.File, importPath string) {
	quoted := strconv.Quote(importPath)
	for _, imp := range file.Imports {
		if imp.Path != nil && imp.Path.Value == quoted {
			return
		}
	}
	spec := &ast.ImportSpec{
		Path: &ast.BasicLit{Kind: token.STRING, Value: quoted},
	}
	file.Imports = append(file.Imports, spec)

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		gen.Specs = append(gen.Specs, spec)
		if len(gen.Specs) > 1 && gen.Lparen == token.NoPos {
			gen.Lparen = 1
		}
		return
	}

	decl := &ast.GenDecl{
		Tok:    token.IMPORT,
		Lparen: 1,
		Specs:  []ast.Spec{spec},
	}
	file.Decls = append([]ast.Decl{decl}, file.Decls...)
}

func insertRegisterCall(file *ast.File, pkgName string) error {
	if countSelectorCalls(file, pkgName, "Register") > 0 {
		return nil
	}

	stmt := &ast.ExprStmt{
		X: &ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   ast.NewIdent(pkgName),
				Sel: ast.NewIdent("Register"),
			},
			Args: []ast.Expr{ast.NewIdent("app")},
		},
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if insertIntoBlock(fn.Body, stmt) {
			return nil
		}
	}
	return fmt.Errorf("resourcegen: no product.Register(app) or framework.Run(app) registration point in %s", serverMainRel)
}

func insertIntoBlock(block *ast.BlockStmt, stmt ast.Stmt) bool {
	if block == nil {
		return false
	}
	afterRegister := -1
	beforeRun := -1
	for i, item := range block.List {
		if stmtAnchorsSelectorCall(item, "", "Register") {
			afterRegister = i + 1
		}
		if stmtAnchorsSelectorCall(item, "framework", "Run") {
			beforeRun = i
		}
	}
	insertAt := -1
	switch {
	case afterRegister >= 0:
		insertAt = afterRegister
	case beforeRun >= 0:
		insertAt = beforeRun
	}
	if insertAt >= 0 {
		list := block.List
		block.List = append(list[:insertAt:insertAt], append([]ast.Stmt{stmt}, list[insertAt:]...)...)
		return true
	}
	for _, item := range block.List {
		for _, child := range nestedBlocks(item) {
			if insertIntoBlock(child, stmt) {
				return true
			}
		}
	}
	return false
}

func nestedBlocks(stmt ast.Stmt) []*ast.BlockStmt {
	switch s := stmt.(type) {
	case *ast.BlockStmt:
		return []*ast.BlockStmt{s}
	case *ast.IfStmt:
		var blocks []*ast.BlockStmt
		if s.Body != nil {
			blocks = append(blocks, s.Body)
		}
		if s.Else != nil {
			blocks = append(blocks, nestedBlocks(s.Else)...)
		}
		return blocks
	case *ast.ForStmt:
		if s.Body != nil {
			return []*ast.BlockStmt{s.Body}
		}
	case *ast.RangeStmt:
		if s.Body != nil {
			return []*ast.BlockStmt{s.Body}
		}
	}
	return nil
}

// stmtAnchorsSelectorCall reports whether stmt itself wraps a selector call,
// including IfStmt.Init and AssignStmt RHS. Nested bodies are ignored so the
// caller can recurse and insert at the inner block.
func stmtAnchorsSelectorCall(stmt ast.Stmt, pkgName, selName string) bool {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		return exprContainsSelectorCall(s.X, pkgName, selName)
	case *ast.AssignStmt:
		for _, rhs := range s.Rhs {
			if exprContainsSelectorCall(rhs, pkgName, selName) {
				return true
			}
		}
	case *ast.IfStmt:
		if s.Init != nil && stmtAnchorsSelectorCall(s.Init, pkgName, selName) {
			return true
		}
		if exprContainsSelectorCall(s.Cond, pkgName, selName) {
			return true
		}
	case *ast.ReturnStmt:
		for _, result := range s.Results {
			if exprContainsSelectorCall(result, pkgName, selName) {
				return true
			}
		}
	}
	return false
}

func exprContainsSelectorCall(expr ast.Expr, pkgName, selName string) bool {
	if expr == nil {
		return false
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found || n == nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSelectorCall(call, pkgName, selName) {
			found = true
			return false
		}
		return true
	})
	return found
}

func insertAutoMigrateArg(file *ast.File, pkgName, typeName string) error {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "AutoMigrate" {
			return true
		}
		found = true
		if hasAutoMigrateModel(call, pkgName, typeName) {
			return false
		}
		call.Args = append(call.Args, &ast.UnaryExpr{
			Op: token.AND,
			X: &ast.CompositeLit{
				Type: &ast.SelectorExpr{
					X:   ast.NewIdent(pkgName),
					Sel: ast.NewIdent(typeName),
				},
			},
		})
		return false
	})
	if !found {
		return fmt.Errorf("resourcegen: %w in %s", errNoAutoMigrate, platformDBRel)
	}
	return nil
}

// errNoAutoMigrate is a platform database.go that calls no AutoMigrate
// method, as in an app whose schema comes from its migrations. The file then
// lists no models; a migration the app runs some other way (auth.Migrate, a
// helper in another file) is not visible in it.
var errNoAutoMigrate = errors.New("no AutoMigrate call")

// CollectAutoMigrateModels returns every `&pkg.Type{}` argument of AutoMigrate
// calls in src, resolved to import path + type name via the file's imports.
// Atlas migrate diff treats that slice as the entire desired schema, so callers
// must pass every registered model — not only the resource being generated.
func CollectAutoMigrateModels(src []byte) ([]migrations.Model, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, platformDBRel, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("resourcegen: parse %s: %w", platformDBRel, err)
	}
	pkgToImport := importPathByName(file)
	var models []migrations.Model
	seen := map[string]struct{}{}
	found := false
	var resolveErr error
	ast.Inspect(file, func(n ast.Node) bool {
		if resolveErr != nil {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "AutoMigrate" {
			return true
		}
		found = true
		for _, arg := range call.Args {
			pkgName, typeName, ok := parseAutoMigrateArg(arg)
			if !ok {
				continue
			}
			importPath, ok := pkgToImport[pkgName]
			if !ok {
				resolveErr = fmt.Errorf("resourcegen: AutoMigrate argument &%s.%s{} has no matching import", pkgName, typeName)
				return false
			}
			key := importPath + "." + typeName
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, migrations.Model{ImportPath: importPath, TypeName: typeName})
		}
		return true
	})
	if resolveErr != nil {
		return nil, resolveErr
	}
	if !found {
		return nil, fmt.Errorf("resourcegen: %w in %s", errNoAutoMigrate, platformDBRel)
	}
	return models, nil
}

func importPathByName(file *ast.File) map[string]string {
	out := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
			if name == "_" || name == "." {
				continue
			}
		} else {
			slash := strings.LastIndex(path, "/")
			name = path[slash+1:]
		}
		out[name] = path
	}
	return out
}

func parseAutoMigrateArg(arg ast.Expr) (pkgName, typeName string, ok bool) {
	unary, ok := arg.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return "", "", false
	}
	lit, ok := unary.X.(*ast.CompositeLit)
	if !ok {
		return "", "", false
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return "", "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return ident.Name, sel.Sel.Name, true
}

func hasAutoMigrateModel(call *ast.CallExpr, pkgName, typeName string) bool {
	for _, arg := range call.Args {
		pkg, typ, ok := parseAutoMigrateArg(arg)
		if ok && pkg == pkgName && typ == typeName {
			return true
		}
	}
	return false
}

func countSelectorCalls(file *ast.File, pkgName, selName string) int {
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isSelectorCall(call, pkgName, selName) {
			count++
		}
		return true
	})
	return count
}

func isSelectorCall(call *ast.CallExpr, pkgName, selName string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != selName {
		return false
	}
	if pkgName == "" {
		return true
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkgName
}

// CountRegisterCalls reports how many `<pkg>.Register(...)` calls appear in src.
func CountRegisterCalls(src []byte, pkgName string) (int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		return 0, err
	}
	return countSelectorCalls(file, pkgName, "Register"), nil
}
