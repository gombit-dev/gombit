// Package resourcecheck guards against a generated resource's persistence schema
// and its generated API create path drifting silently into an invalid state
// (#218). `gombit make resource` generates a Huma handler whose create mapping is
// a snapshot of the fields given at generation time; because generated handlers
// are human-owned, the generator cannot rewrite them when the model later gains a
// column. Left unchecked, a NOT NULL column the handler doesn't assign is
// silently zero-filled on create, and nothing catches it until production.
//
// The invariant the generated `*_drift_test.go` asserts is tied to the handler's
// ACTUAL write path — the fields assigned in the create constructor — not merely
// to the request DTO's declared fields. A field can appear in the DTO yet never
// be assigned to the model; that is exactly the silent zero-fill #218 is about.
package resourcecheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"sync"

	"gorm.io/gorm/schema"
)

// MissingCreateColumns reports the persistence-required columns of model that the
// create handler does not actually write. A required column is one that is
// NOT NULL with no database default and is not auto-managed (primary key,
// auto-increment, auto create/update timestamp, or soft-delete). Such a column is
// satisfied — and so omitted from the result — when it is either:
//
//   - assigned by the handler's create constructor (its Go field name is in
//     assignedGoFields, from ConstructorCreateFields); or
//   - explicitly server-managed (its column is listed in serverManaged, e.g. a
//     tenant or owner id set from the auth context).
//
// model is a pointer to the GORM model (e.g. &Widget{}); assignedGoFields are the
// struct fields the create handler assigns; serverManaged lists columns filled
// server-side. The returned column names are sorted.
//
// Only the create side is checked: a response DTO may legitimately omit fields
// (intentional encapsulation), but a create path that cannot supply a
// persistence-required column violates a schema invariant.
func MissingCreateColumns(model any, assignedGoFields []string, serverManaged []string) ([]string, error) {
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return nil, fmt.Errorf("resourcecheck: parse model schema: %w", err)
	}
	assigned := toSet(assignedGoFields)
	managed := toSet(serverManaged)

	var missing []string
	for _, f := range sch.Fields {
		if !requiresCreateValue(f) {
			continue
		}
		if assigned[f.Name] || managed[f.DBName] {
			continue
		}
		missing = append(missing, f.DBName)
	}
	sort.Strings(missing)
	return missing, nil
}

// ConstructorCreateFields parses a generated feature-package handler source and
// returns the model struct fields the create constructor is GUARANTEED to
// persist — the keys common to every `<typeName>{ ... }` composite literal
// RETURNED by the function named funcName (e.g. buildWidgetForCreate), which the
// handler passes straight to GORM's Create.
//
// Because only returned values are inspected — not every literal syntactically
// present under the function — a decoy or temporary `<typeName>{...}` that is
// never returned does not falsely certify a field, and a literal in some other
// function (or in a nested closure, whose returns are not the ctor's) is ignored.
//
// When the constructor has several return paths, the result is their
// INTERSECTION, not their union: a field set on only one branch is NOT certified,
// because another path would persist its zero value. Any return path that is not
// an inline `<typeName>{...}` literal (a named local, a helper call) contributes
// the empty set, which empties the intersection — so such a hand-refactor fails
// closed, surfacing every required column as drift to resolve explicitly rather
// than a silent gap.
func ConstructorCreateFields(handlerSrc []byte, funcName, typeName string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handler.go", handlerSrc, 0)
	if err != nil {
		return nil, fmt.Errorf("resourcecheck: parse handler: %w", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if ok && d.Name != nil && d.Name.Name == funcName && d.Body != nil {
			fn = d
			break
		}
	}
	if fn == nil {
		return nil, nil
	}

	// One key-set per return path, skipping nested function literals (their
	// returns belong to the closure, not the constructor).
	var perReturn []map[string]bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		keys := map[string]bool{}
		for _, res := range ret.Results {
			collectLiteralKeys(res, typeName, keys)
		}
		perReturn = append(perReturn, keys)
		return true
	})

	set := intersectKeys(perReturn)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// intersectKeys returns the keys present in every set. With no sets it returns
// an empty map (nothing certified), which fails closed.
func intersectKeys(sets []map[string]bool) map[string]bool {
	out := map[string]bool{}
	if len(sets) == 0 {
		return out
	}
	for k := range sets[0] {
		out[k] = true
	}
	for _, s := range sets[1:] {
		for k := range out {
			if !s[k] {
				delete(out, k)
			}
		}
	}
	return out
}

// CreatePersistsConstructor reports whether the handler's create method persists
// the value returned by ctorName UNCHANGED, closing the gap between "the
// constructor returns these fields" and "these fields are what GORM persists".
//
// Syntactic presence of a constructor assignment and a Create call is not
// enough: the assignment must actually REACH the persistence call at runtime.
// The check therefore validates the method's flat top-level statement sequence
// (the shape `gombit make resource` generates), establishing ordering and
// dominance conservatively rather than merely finding both spellings somewhere
// in the syntax tree. All of the following must hold, or it returns false with a
// detail explaining why:
//
//   - Exactly one `…Create(x)` call exists in the whole method, its argument is a
//     plain variable (`&row` or `row`), and that call sits inside a single
//     top-level statement (not nested in a closure).
//   - That variable is assigned exactly once in the whole method; the assignment
//     is a top-level statement of the form `row := ctorName(...)` /
//     `row = ctorName(...)` and is never field-mutated (`row.X = …`) anywhere.
//   - The constructor assignment's top-level position precedes the Create call's
//     top-level position (dominance on the generated straight-line body).
//
// Anything else — building the value inline, mutating it, reassigning it,
// assigning it under a conditional or inside a closure, writing it after Create,
// or no Create at all — makes the persisted value diverge from what
// ConstructorCreateFields inspected, so the guard (which treats false as a
// failure) rejects it: the create path must route straight through the
// constructor for the guarantee to hold.
func CreatePersistsConstructor(handlerSrc []byte, createFuncName, ctorName string) (bool, string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handler.go", handlerSrc, 0)
	if err != nil {
		return false, "", fmt.Errorf("resourcecheck: parse handler: %w", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if ok && d.Name != nil && d.Name.Name == createFuncName && d.Body != nil {
			fn = d
			break
		}
	}
	if fn == nil {
		return false, "no " + createFuncName + " method with a body was found", nil
	}

	// Exactly one `…Create(x)` call must exist anywhere in the method (a second,
	// possibly hidden, Create would make "the persisted value" ambiguous).
	creates := collectCreateCalls(fn.Body)
	if len(creates) != 1 {
		return false, fmt.Sprintf("expected exactly one DB.Create(...) call in %s, found %d", createFuncName, len(creates)), nil
	}
	createArg := identName(creates[0].Args[0])
	if createArg == "" {
		return false, "the DB.Create(...) argument is not a plain &row/row variable", nil
	}

	// Aliasing: the address of the persisted variable may be taken ONLY as the
	// Create argument. Any other `&row` could hand an alias to code that mutates
	// the row before it is persisted (`p := &row; p.X = …`), which this AST check
	// cannot follow — so it fails closed.
	createAddr := creates[0].Args[0]
	aliasLeak := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		u, ok := n.(*ast.UnaryExpr)
		if !ok || u.Op != token.AND || u == createAddr {
			return true
		}
		if id, ok := u.X.(*ast.Ident); ok && id.Name == createArg {
			aliasLeak = true
		}
		return true
	})
	if aliasLeak {
		return false, "the address of " + createArg + " is taken outside the Create call; an alias could mutate the persisted value", nil
	}

	// The persisted variable must be assigned exactly once, and never
	// field-mutated, across the WHOLE method (descending into closures/dead code
	// so a hidden write still disqualifies it).
	assignCount := 0
	var ctorAssign *ast.AssignStmt
	mutated := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			switch target := lhs.(type) {
			case *ast.Ident:
				if target.Name == createArg {
					assignCount++
					if i < len(as.Rhs) && isCallTo(as.Rhs[i], ctorName) {
						ctorAssign = as
					}
				}
			case *ast.SelectorExpr:
				if x, ok := target.X.(*ast.Ident); ok && x.Name == createArg {
					mutated = true // row.Field = … changes the persisted value
				}
			}
		}
		return true
	})
	if mutated {
		return false, createArg + " is field-mutated before Create; route the create solely through " + ctorName, nil
	}
	if assignCount != 1 || ctorAssign == nil {
		return false, createArg + " must be assigned exactly once, from " + ctorName + "(...)", nil
	}

	// Ordering + dominance on the generated straight-line body: the constructor
	// assignment and the Create call must each be a TOP-LEVEL statement, with the
	// assignment strictly before the Create. A constructor assignment nested in a
	// conditional, loop, or closure (non-dominating) leaves ctorIdx == -1 and is
	// rejected, as does a Create that runs before the assignment.
	ctorIdx, createIdx := -1, -1
	for i, stmt := range fn.Body.List {
		if stmt == ctorAssign {
			ctorIdx = i
		}
		if topLevelStmtHasCreateCall(stmt) {
			createIdx = i
		}
	}
	if ctorIdx < 0 {
		return false, "the " + ctorName + "(...) assignment is not a top-level statement (it must dominate Create, not sit under a branch/loop/closure)", nil
	}
	if createIdx < 0 {
		return false, "the Create call is not reached by a top-level statement (it must not be buried in a closure)", nil
	}
	if ctorIdx >= createIdx {
		return false, createArg + " is assigned from " + ctorName + " only at or after the Create call; the constructor result never reaches persistence", nil
	}
	return true, "", nil
}

// collectCreateCalls returns every `<expr>.Create(<one arg>)` call under n,
// descending into nested blocks and closures so a second, hidden Create is still
// counted.
func collectCreateCalls(n ast.Node) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "Create" && len(call.Args) == 1 {
			out = append(out, call)
		}
		return true
	})
	return out
}

// topLevelStmtHasCreateCall reports whether a single top-level statement reaches
// a `…Create(...)` call without crossing a function-literal boundary. It is how
// dominance is established: a Create inside a closure is NOT reached by the
// enclosing top-level statement, so it does not count.
func topLevelStmtHasCreateCall(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false // do not descend into closures
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "Create" && len(call.Args) == 1 {
			found = true
			return false
		}
		return true
	})
	return found
}

// identName returns the identifier name of `x` or `&x`, else "".
func identName(expr ast.Expr) string {
	if u, ok := expr.(*ast.UnaryExpr); ok {
		expr = u.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// isCallTo reports whether expr is a call to the function named funcName.
func isCallTo(expr ast.Expr, funcName string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == funcName
}

// collectLiteralKeys records the keyed field names of a returned
// `<typeName>{ ... }` (or `&<typeName>{ ... }`) composite literal.
func collectLiteralKeys(expr ast.Expr, typeName string, set map[string]bool) {
	if u, ok := expr.(*ast.UnaryExpr); ok {
		expr = u.X
	}
	cl, ok := expr.(*ast.CompositeLit)
	if !ok {
		return
	}
	id, ok := cl.Type.(*ast.Ident)
	if !ok || id.Name != typeName {
		return
	}
	for _, elt := range cl.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok {
				set[key.Name] = true
			}
		}
	}
}

// requiresCreateValue reports whether a column must be given a value on create:
// NOT NULL, no DB default, and not something the database or GORM fills itself.
func requiresCreateValue(f *schema.Field) bool {
	if f == nil || !f.NotNull || f.HasDefaultValue {
		return false
	}
	if f.PrimaryKey || f.AutoIncrement {
		return false
	}
	if f.AutoCreateTime != 0 || f.AutoUpdateTime != 0 {
		return false
	}
	// gorm.DeletedAt is nullable, but guard explicitly in case a model aliases it.
	if f.Name == "DeletedAt" {
		return false
	}
	return true
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}
