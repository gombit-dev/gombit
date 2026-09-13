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

// CreatePersistsConstructor reports whether receiverType's createFuncName method
// persists the value returned by ctorName, modified ONLY by assignments to
// explicitly declared server-managed columns, and by nothing else — closing the
// gap between "the constructor returns these fields" and "these fields are what
// GORM persists".
//
// Syntactic presence of a constructor assignment and a Create call is not enough:
// the assignment must REACH the persistence call at runtime. Because the
// generated create method is a fixed, flat statement sequence, the check
// validates an allowlist of the permitted uses of the persisted variable rather
// than attempting general value-flow analysis. All of the following must hold, or
// it returns false with a detail explaining why:
//
//   - The method belongs to receiverType (e.g. *Handler); a same-named method on
//     another type is not the registered handler and cannot certify it.
//   - Exactly one persistence call `<recv>.….Create(x)` exists, rooted at the
//     method receiver (so an unrelated `.Create` cannot masquerade as GORM's),
//     its argument a plain `&row`/`row`, reached by a single top-level statement.
//   - row is assigned exactly once, a top-level `row := ctorName(...)` that
//     strictly precedes the Create (ordering + dominance on the flat body).
//   - The ONLY other pre-Create uses of row are assignments `row.F = …` whose
//     column F is in serverManagedColumns. Every other use before Create — a
//     non-managed field assignment, an `x++`, a method call `row.M()` that may
//     take &row implicitly, taking `&row` anywhere but the Create argument, or a
//     read — is rejected. Uses after Create (response mapping) are free.
//
// serverManagedColumns holds DB column names (e.g. "tenant_id"); a row field is
// matched to it with GORM's default naming strategy. Listing a column permits the
// assignment but does not prove it runs (a BeforeCreate hook is a valid source);
// the list is the human's explicit assertion, and MissingCreateColumns still
// treats it as a known source. Anything the allowlist cannot account for fails
// closed, surfacing as drift to resolve explicitly rather than a silent gap.
func CreatePersistsConstructor(handlerSrc []byte, receiverType, createFuncName, ctorName string, serverManagedColumns []string) (bool, string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handler.go", handlerSrc, 0)
	if err != nil {
		return false, "", fmt.Errorf("resourcecheck: parse handler: %w", err)
	}

	// Find createFuncName declared on the expected receiver. A decoy method of the
	// same name on another type (which the router never calls) must not certify
	// the real handler, so the receiver is matched explicitly.
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name == nil || d.Name.Name != createFuncName || d.Body == nil {
			continue
		}
		if receiverTypeName(d) != receiverType {
			continue
		}
		if fn != nil {
			return false, "multiple " + receiverType + "." + createFuncName + " methods found", nil
		}
		fn = d
	}
	if fn == nil {
		return false, "no " + receiverType + "." + createFuncName + " method with a body was found", nil
	}
	recvVar := receiverVarName(fn)
	if recvVar == "" {
		return false, receiverType + "." + createFuncName + " has no named receiver", nil
	}

	// Exactly one persistence call, rooted at the receiver: `<recvVar>.….Create(x)`.
	var createCall *ast.CallExpr
	createCount := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Create" || len(call.Args) != 1 {
			return true
		}
		if rootIdentName(sel.X) != recvVar {
			return true // some other object's .Create, not the handler's DB
		}
		createCall = call
		createCount++
		return true
	})
	if createCount != 1 {
		return false, fmt.Sprintf("expected exactly one %s.….Create(&row) call in %s, found %d", recvVar, createFuncName, createCount), nil
	}

	createAddr := createCall.Args[0]
	createArg := identName(createAddr)
	createArgIdent := argIdent(createAddr)
	if createArg == "" || createArgIdent == nil {
		return false, "the Create(...) argument is not a plain &row/row variable", nil
	}

	ns := schema.NamingStrategy{}
	managed := toSet(serverManagedColumns)

	// Walk assignments and increments touching row. row itself must be assigned
	// exactly once (from the constructor); row.F may be assigned only when column
	// F is server-managed; increments of row/row.F are never allowed.
	assignCount := 0
	var ctorAssign *ast.AssignStmt
	var ctorLHSIdent *ast.Ident
	permittedFieldIdents := map[*ast.Ident]bool{}
	reason := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if reason != "" {
			return false
		}
		switch s := n.(type) {
		case *ast.IncDecStmt:
			if touchesVar(s.X, createArg) {
				reason = createArg + " is incremented/decremented; the persisted value must come from " + ctorName
			}
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				switch target := lhs.(type) {
				case *ast.Ident:
					if target.Name == createArg {
						assignCount++
						if i < len(s.Rhs) && isCallTo(s.Rhs[i], ctorName) {
							ctorAssign = s
							ctorLHSIdent = target
						}
					}
				case *ast.SelectorExpr:
					if x, ok := target.X.(*ast.Ident); ok && x.Name == createArg {
						col := ns.ColumnName("", target.Sel.Name)
						if managed[col] {
							permittedFieldIdents[x] = true
						} else {
							reason = createArg + "." + target.Sel.Name + " is assigned but column " + col + " is not server-managed; set it in " + ctorName + " or declare it server-managed"
						}
					}
				}
			}
		}
		return true
	})
	if reason != "" {
		return false, reason, nil
	}
	if assignCount != 1 || ctorAssign == nil {
		return false, createArg + " must be assigned exactly once, from " + ctorName + "(...)", nil
	}

	// Ordering + dominance on the flat generated body: the constructor assignment
	// and the Create call must each be a TOP-LEVEL statement, assignment first. A
	// constructor assignment nested in a branch/loop/closure leaves ctorIdx == -1.
	ctorIdx, createIdx := -1, -1
	for i, stmt := range fn.Body.List {
		if stmt == ctorAssign {
			ctorIdx = i
		}
		if stmtReachesNode(stmt, createCall) {
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

	// Allowlist every use of row before the Create call: it must be the
	// constructor assignment's LHS, a permitted server-managed field assignment's
	// receiver, or the &row handed to Create. Anything else before Create — a
	// method call `row.clearTenant()` that implicitly takes &row, a stray `&row`,
	// an unexpected read — is rejected. Uses after Create are post-persist reads.
	stray := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if stray != "" {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != createArg {
			return true
		}
		if id == ctorLHSIdent || id == createArgIdent || permittedFieldIdents[id] {
			return true
		}
		if id.Pos() > createCall.End() {
			return true // post-persist read (e.g. response mapping)
		}
		stray = "an unrecognized use of " + createArg + " appears before Create (e.g. a method call, read, or indirect mutation); the generated grammar permits only construction, server-managed field assignments, and Create(&" + createArg + ")"
		return false
	})
	if stray != "" {
		return false, stray, nil
	}
	return true, "", nil
}

// receiverTypeName returns fn's receiver type name without a leading pointer
// (e.g. "Handler" for `(h *Handler)`), or "" if fn has no single-type receiver.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// receiverVarName returns fn's receiver variable name (the "h" in `(h *Handler)`).
func receiverVarName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// rootIdentName walks a selector/call chain to its leftmost identifier
// (`h.DB.WithContext(ctx).Create` → "h"), or "" if it is not ident-rooted.
func rootIdentName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.CallExpr:
			expr = e.Fun
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		default:
			return ""
		}
	}
}

// argIdent returns the identifier inside `&x` or a bare `x`, else nil.
func argIdent(expr ast.Expr) *ast.Ident {
	if u, ok := expr.(*ast.UnaryExpr); ok {
		expr = u.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id
	}
	return nil
}

// touchesVar reports whether expr is `name` or `name.Field` (an inc/dec target
// that would mutate the variable).
func touchesVar(expr ast.Expr, name string) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == name
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && x.Name == name
	}
	return false
}

// stmtReachesNode reports whether target appears within stmt without crossing a
// function-literal boundary (so a Create inside a closure is not "reached" by the
// enclosing top-level statement).
func stmtReachesNode(stmt ast.Stmt, target ast.Node) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok && n != target {
			return false
		}
		if n == target {
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

// ModelContractDrift compares a model's content columns against the generated
// create request DTO and the response DTO, detecting the broader model↔handler
// divergence #218 is about (not just the silent zero-fill of required columns).
//
// A content column is any persisted column that is not auto-managed (primary
// key, auto create/update timestamp, soft-delete) — including NULLABLE and
// DATABASE-DEFAULTED columns, which MissingCreateColumns deliberately skips.
//
//   - writeDrift: content columns a client cannot set, because they are absent
//     from the create request DTO (createGoFields) and are neither server-managed
//     nor write-exempt. Adding such a column to the model without updating the
//     handler's create DTO is exactly the "422 unexpected property" divergence in
//     #218: the client cannot supply the field the model now expects.
//   - readDrift: content columns never surfaced, because they are absent from the
//     response DTO (responseGoFields) and are not read-exempt.
//
// createGoFields/responseGoFields are Go field names (parsed from the handler's
// DTO structs); serverManagedCols/writeExemptCols/readExemptCols are DB column
// names (the human-owned opt-out lists). Returned drift is DB column names,
// sorted. The write/read exempt lists are the explicit opt-outs for intentional
// omissions, so an encapsulated field fails closed only until a human declares it.
func ModelContractDrift(model any, createGoFields, responseGoFields, serverManagedCols, writeExemptCols, readExemptCols []string) (writeDrift, readDrift []string, err error) {
	sch, perr := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if perr != nil {
		return nil, nil, fmt.Errorf("resourcecheck: parse model schema: %w", perr)
	}
	createGo := toSet(createGoFields)
	responseGo := toSet(responseGoFields)
	serverDB := toSet(serverManagedCols)
	writeDB := toSet(writeExemptCols)
	readDB := toSet(readExemptCols)

	for _, f := range sch.Fields {
		if !isContentField(f) {
			continue
		}
		if !createGo[f.Name] && !serverDB[f.DBName] && !writeDB[f.DBName] {
			writeDrift = append(writeDrift, f.DBName)
		}
		if !responseGo[f.Name] && !readDB[f.DBName] {
			readDrift = append(readDrift, f.DBName)
		}
	}
	sort.Strings(writeDrift)
	sort.Strings(readDrift)
	return writeDrift, readDrift, nil
}

// isContentField reports whether a column is part of the resource's editable/
// visible surface — everything except the framework-managed primary key, auto
// timestamps, and soft-delete. Unlike requiresCreateValue it keeps nullable and
// database-defaulted columns, since those still belong in the API contract.
func isContentField(f *schema.Field) bool {
	if f == nil {
		return false
	}
	if f.PrimaryKey || f.AutoIncrement {
		return false
	}
	if f.AutoCreateTime != 0 || f.AutoUpdateTime != 0 {
		return false
	}
	if f.Name == "DeletedAt" {
		return false
	}
	return true
}

// DTOFieldNames returns the Go field names declared by the named struct type
// (e.g. the response DTO "bookData"), or nil if no such struct is found.
func DTOFieldNames(handlerSrc []byte, typeName string) ([]string, error) {
	file, err := parseHandler(handlerSrc)
	if err != nil {
		return nil, err
	}
	st := findStructType(file, typeName)
	if st == nil {
		return nil, nil
	}
	return structFieldNames(st), nil
}

// RequestBodyFieldNames returns the Go field names of the `Body` field of the
// named request-input type (e.g. "createBookInput"). Body may be an inline struct
// literal or a named struct type; either is resolved to its fields.
func RequestBodyFieldNames(handlerSrc []byte, inputTypeName string) ([]string, error) {
	file, err := parseHandler(handlerSrc)
	if err != nil {
		return nil, err
	}
	st := findStructType(file, inputTypeName)
	if st == nil {
		return nil, nil
	}
	var bodyType ast.Expr
	for _, field := range st.Fields.List {
		for _, nm := range field.Names {
			if nm.Name == "Body" {
				bodyType = field.Type
			}
		}
	}
	if bodyType == nil {
		return nil, nil
	}
	return fieldNamesOfType(file, bodyType), nil
}

func parseHandler(handlerSrc []byte) (*ast.File, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "handler.go", handlerSrc, 0)
	if err != nil {
		return nil, fmt.Errorf("resourcecheck: parse handler: %w", err)
	}
	return file, nil
}

// findStructType returns the struct type declared as `type <name> struct {…}`.
func findStructType(file *ast.File, name string) *ast.StructType {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil || ts.Name.Name != name {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				return st
			}
		}
	}
	return nil
}

// fieldNamesOfType resolves an inline struct, a named struct type, or a pointer
// to either, to its declared field names.
func fieldNamesOfType(file *ast.File, t ast.Expr) []string {
	switch ft := t.(type) {
	case *ast.StructType:
		return structFieldNames(ft)
	case *ast.StarExpr:
		return fieldNamesOfType(file, ft.X)
	case *ast.Ident:
		if st := findStructType(file, ft.Name); st != nil {
			return structFieldNames(st)
		}
	}
	return nil
}

// structFieldNames returns the declared Go field names of a struct type, sorted.
// Named fields contribute their name; an embedded field contributes its type name.
func structFieldNames(st *ast.StructType) []string {
	var out []string
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			if name := embeddedName(field.Type); name != "" {
				out = append(out, name)
			}
			continue
		}
		for _, nm := range field.Names {
			if nm.Name != "_" {
				out = append(out, nm.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// embeddedName returns the type name of an embedded field (`T`, `*T`, `pkg.T`).
func embeddedName(t ast.Expr) string {
	switch e := t.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return embeddedName(e.X)
	case *ast.SelectorExpr:
		if e.Sel != nil {
			return e.Sel.Name
		}
	}
	return ""
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
