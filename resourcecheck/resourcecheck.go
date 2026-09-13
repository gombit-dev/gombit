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
	"reflect"
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
	file, err := parseHandler(handlerSrc)
	if err != nil {
		return nil, err
	}
	// Only the package-level function (Recv == nil) — the one an unqualified
	// call resolves to — is inspected; a same-named method decoy is ignored.
	fn := packageFunc(file, funcName)
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

// ValidateCreateGrammar is a fail-closed validator of the ONE canonical create
// grammar that `gombit make resource` emits — deliberately NOT a dataflow or
// alias analysis. It accepts only a create method structurally equivalent to:
//
//	func (h *Handler) create(ctx context.Context, input *createXInput) (*createXOutput, error) {
//		row := buildXForCreate(ctx, input)
//		if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil { ... }
//		return ..., nil
//	}
//
// where buildXForCreate is a single unconditional `return X{...}` literal.
// Because the constructor is context-aware, every persisted value — including
// server-derived columns like a tenant id — is set INSIDE that literal, so the
// value GORM writes is exactly the literal the required-column and contract
// checks inspect. No post-construction mutation of row is permitted.
//
// Anything outside this shape — a branch or loop, an alias or `&row` elsewhere, a
// `defer`/`go` Create, a pointer-receiver method call on row, `row.F =`/`row.F++`,
// an extra or non-`h.DB` Create, a reordering — makes those static checks unsound,
// so it is rejected with "unsupported create-handler shape: …". The generated
// handler is human-owned: if you must diverge, the test tells you the guarantee
// no longer holds rather than pretending to have proved your refactor safe.
func ValidateCreateGrammar(handlerSrc []byte, receiverType, createMethod, ctorName, typeName string) (bool, string, error) {
	file, err := parseHandler(handlerSrc)
	if err != nil {
		return false, "", err
	}

	// The constructor must be a single unconditional `return <typeName>{...}`.
	if ok, detail := validateConstructor(file, ctorName, typeName); !ok {
		return false, detail, nil
	}

	// Exactly one create method, on the expected receiver (a same-named method on
	// another type is not the registered handler).
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name == nil || d.Name.Name != createMethod || d.Body == nil {
			continue
		}
		if receiverTypeName(d) != receiverType {
			continue
		}
		if fn != nil {
			return false, "unsupported create-handler shape: multiple " + receiverType + "." + createMethod + " methods", nil
		}
		fn = d
	}
	if fn == nil {
		return false, "unsupported create-handler shape: no " + receiverType + "." + createMethod + " method", nil
	}
	recvVar := receiverVarName(fn)
	if recvVar == "" {
		return false, "unsupported create-handler shape: " + createMethod + " has no named receiver", nil
	}

	// The body must be EXACTLY the three generated statements, in order:
	//
	//	row := <ctorName>(ctx, input)
	//	if err := <recv>.DB.….Create(&row).Error; err != nil { … }
	//	return …
	//
	// Recognizing the whole program — rather than searching a permissive AST for a
	// compatible Create somewhere — is what makes this fail closed: a conditional
	// or unchecked Create, an early return, a reorder, or any extra statement
	// (a mutation, a second Create) changes the statement list and is rejected.
	shapeErr := "unsupported create-handler shape: the create method must be exactly `row := " +
		ctorName + "(ctx, input)`, then `if err := " + recvVar +
		".DB.….Create(&row).Error; err != nil { … }`, then a single return"
	body := fn.Body.List
	if len(body) != 3 {
		return false, shapeErr, nil
	}

	// Statement 1: row := <ctorName>(...).
	assign, ok := body[0].(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false, shapeErr, nil
	}
	rowIdent, ok := assign.Lhs[0].(*ast.Ident)
	if !ok || !isCallTo(assign.Rhs[0], ctorName) {
		return false, shapeErr, nil
	}
	row := rowIdent.Name

	// Statement 2: the checked-error Create, exactly
	// `if err := <recv>.DB.WithContext(<ctx>).Create(&row).Error; err != nil { return …err… }`.
	ctxName := firstParamName(fn)
	ifStmt, ok := body[1].(*ast.IfStmt)
	if ctxName == "" || !ok || !isCheckedDBCreate(ifStmt, recvVar, ctxName, row) {
		return false, shapeErr, nil
	}

	// Statement 3: a return (the response). row is not mutable here — a single
	// ReturnStmt cannot reassign it, and any mutating statement would have made
	// the body longer than three.
	if _, ok := body[2].(*ast.ReturnStmt); !ok {
		return false, shapeErr, nil
	}
	return true, "", nil
}

// validateConstructor requires ctorName to be a single package-level function
// (not a method — Recv == nil) whose only statement is `return <typeName>{...}`.
// Requiring Recv == nil closes a decoy where a same-named method is inspected
// while the handler's unqualified call resolves to the package function.
func validateConstructor(file *ast.File, ctorName, typeName string) (bool, string) {
	fn := packageFunc(file, ctorName)
	if fn == nil {
		return false, "unsupported constructor shape: no package-level " + ctorName + " function"
	}
	if len(fn.Body.List) != 1 {
		return false, "unsupported constructor shape: " + ctorName + " must be a single `return " + typeName + "{...}` statement"
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false, "unsupported constructor shape: " + ctorName + " must return exactly one value"
	}
	cl, ok := ret.Results[0].(*ast.CompositeLit)
	if !ok {
		return false, "unsupported constructor shape: " + ctorName + " must return a " + typeName + "{...} literal directly"
	}
	if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != typeName {
		return false, "unsupported constructor shape: " + ctorName + " must return a " + typeName + "{...} literal"
	}
	return true, ""
}

// packageFunc returns the single package-level function (Recv == nil) named name,
// or nil if there is none or more than one (an ambiguity fails closed).
func packageFunc(file *ast.File, name string) *ast.FuncDecl {
	var found *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Recv != nil || d.Name == nil || d.Name.Name != name || d.Body == nil {
			continue
		}
		if found != nil {
			return nil
		}
		found = d
	}
	return found
}

// isCheckedDBCreate reports whether ifStmt is EXACTLY the generated checked
// persistence statement:
//
//	if err := <recv>.DB.WithContext(<ctx>).Create(&row).Error; err != nil {
//		return …err…
//	}
//
// The chain is pinned to `<recv>.DB.WithContext(<ctx>)` with no intervening call,
// so `h.DB.Session(&gorm.Session{DryRun:true})` (a successful non-write) does not
// match; there must be no `else` (so it cannot delete the row on the success
// path); and the error branch must return the error (so `return nil, nil` cannot
// report success when Create failed).
func isCheckedDBCreate(ifStmt *ast.IfStmt, recvVar, ctxName, row string) bool {
	if ifStmt.Else != nil {
		return false
	}
	init, ok := ifStmt.Init.(*ast.AssignStmt)
	if !ok || init.Tok != token.DEFINE || len(init.Lhs) != 1 || len(init.Rhs) != 1 {
		return false
	}
	errIdent, ok := init.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}
	// RHS: <recv>.DB.WithContext(<ctx>).Create(&row).Error
	dotError, ok := init.Rhs[0].(*ast.SelectorExpr)
	if !ok || dotError.Sel == nil || dotError.Sel.Name != "Error" {
		return false
	}
	createCall, ok := dotError.X.(*ast.CallExpr)
	if !ok || len(createCall.Args) != 1 {
		return false
	}
	createSel, ok := createCall.Fun.(*ast.SelectorExpr)
	if !ok || createSel.Sel == nil || createSel.Sel.Name != "Create" {
		return false
	}
	if !isReceiverDBWithContext(createSel.X, recvVar, ctxName) {
		return false
	}
	amp, ok := createCall.Args[0].(*ast.UnaryExpr)
	if !ok || amp.Op != token.AND {
		return false
	}
	if id, ok := amp.X.(*ast.Ident); !ok || id.Name != row {
		return false
	}
	// Cond: err != nil (in either operand order).
	cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return false
	}
	errVsNil := (isIdent(cond.X, errIdent.Name) && isNilIdent(cond.Y)) ||
		(isIdent(cond.Y, errIdent.Name) && isNilIdent(cond.X))
	if !errVsNil {
		return false
	}
	// The error branch must be a single return that references err (not a bare
	// `return nil, nil` that reports success when Create failed).
	if ifStmt.Body == nil || len(ifStmt.Body.List) != 1 {
		return false
	}
	ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
	return ok && exprsReference(ret.Results, errIdent.Name)
}

// isReceiverDBWithContext reports whether expr is exactly
// `<recv>.DB.WithContext(<ctx>)`, with no intervening session/scope call.
func isReceiverDBWithContext(expr ast.Expr, recvVar, ctxName string) bool {
	withCtx, ok := expr.(*ast.CallExpr)
	if !ok || len(withCtx.Args) != 1 {
		return false
	}
	if id, ok := withCtx.Args[0].(*ast.Ident); !ok || id.Name != ctxName {
		return false
	}
	wcSel, ok := withCtx.Fun.(*ast.SelectorExpr)
	if !ok || wcSel.Sel == nil || wcSel.Sel.Name != "WithContext" {
		return false
	}
	dbSel, ok := wcSel.X.(*ast.SelectorExpr)
	if !ok || dbSel.Sel == nil || dbSel.Sel.Name != "DB" {
		return false
	}
	id, ok := dbSel.X.(*ast.Ident)
	return ok && id.Name == recvVar
}

// exprsReference reports whether name appears as an identifier anywhere in exprs.
func exprsReference(exprs []ast.Expr, name string) bool {
	found := false
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return !found
		})
	}
	return found
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

func isNilIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "nil"
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

// firstParamName returns the name of fn's first parameter (the "ctx"), or "".
func firstParamName(fn *ast.FuncDecl) string {
	if fn.Type == nil || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return ""
	}
	names := fn.Type.Params.List[0].Names
	if len(names) == 0 {
		return ""
	}
	return names[0].Name
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

// ModelWireDrift compares a model's DB-backed content columns against the JSON
// property names the handler's create request and read response ACTUALLY expose —
// the real wire contract, detecting the broader model↔handler divergence #218 is
// about (not just the silent zero-fill of required columns).
//
// createHandler and responseHandler are the Huma handler funcs the routes
// register (pass h.create and h.get). Their request/response Go types are read by
// reflection, so a stale, renamed, or swapped DTO is caught — the check follows
// the types the handler really uses, not a conventionally named declaration. JSON
// tags are honored exactly as Huma serializes them: `json:"-"` is excluded, a
// renamed tag uses the wire name, embedded structs are flattened, and the D10
// `{"data": …}` envelope is unwrapped for the response.
//
// A content column is any DB-backed column (non-empty column name — so GORM
// relationship/association fields are excluded) that is not auto-managed (primary
// key, auto timestamps, soft-delete), INCLUDING nullable and database-defaulted
// columns that MissingCreateColumns skips.
//
//   - writeDrift: content columns absent from the create request body and neither
//     server-managed nor write-exempt — the "422 unexpected property" class: a
//     client cannot supply the field the model now expects.
//   - readDrift: content columns absent from the response and not read-exempt.
//
// serverManagedCols/writeExemptCols/readExemptCols are DB column names (the
// human-owned opt-out lists), compared against the model's own Field.DBName so one
// column name is authoritative across every check. Returned drift is DB column
// names, sorted.
func ModelWireDrift(model, createHandler, responseHandler any, serverManagedCols, writeExemptCols, readExemptCols []string) (writeDrift, readDrift []string, err error) {
	sch, perr := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if perr != nil {
		return nil, nil, fmt.Errorf("resourcecheck: parse model schema: %w", perr)
	}
	reqProps := toSet(requestWireProps(createHandler))
	respProps := toSet(responseWireProps(responseHandler))
	serverDB := toSet(serverManagedCols)
	writeDB := toSet(writeExemptCols)
	readDB := toSet(readExemptCols)

	for _, f := range sch.Fields {
		if !isContentField(f) {
			continue
		}
		if !reqProps[f.DBName] && !serverDB[f.DBName] && !writeDB[f.DBName] {
			writeDrift = append(writeDrift, f.DBName)
		}
		if !respProps[f.DBName] && !readDB[f.DBName] {
			readDrift = append(readDrift, f.DBName)
		}
	}
	sort.Strings(writeDrift)
	sort.Strings(readDrift)
	return writeDrift, readDrift, nil
}

// isContentField reports whether a column is part of the resource's editable/
// visible surface — a DB-backed column (relationship/association fields carry no
// column name and are excluded) that is not the framework-managed primary key,
// an auto timestamp, or soft-delete. Unlike requiresCreateValue it keeps nullable
// and database-defaulted columns, which still belong in the API contract.
func isContentField(f *schema.Field) bool {
	if f == nil || f.DBName == "" {
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

// requestWireProps returns the JSON property names of a Huma handler's request
// body — reflect over the handler func's last input (`*createXInput`), its `Body`
// field, honoring json tags.
func requestWireProps(handler any) []string {
	ft := reflect.TypeOf(handler)
	if ft == nil || ft.Kind() != reflect.Func || ft.NumIn() == 0 {
		return nil
	}
	return bodyWireProps(ft.In(ft.NumIn()-1), false)
}

// responseWireProps returns the JSON property names of a Huma handler's response
// body — reflect over the handler func's first output (`*getXOutput`), its `Body`
// field, then unwrap the D10 `{"data": …}` envelope.
func responseWireProps(handler any) []string {
	ft := reflect.TypeOf(handler)
	if ft == nil || ft.Kind() != reflect.Func || ft.NumOut() == 0 {
		return nil
	}
	return bodyWireProps(ft.Out(0), true)
}

// bodyWireProps resolves a Huma input/output wrapper type to the JSON property
// names of its `Body`, optionally unwrapping the D10 data envelope.
func bodyWireProps(t reflect.Type, unwrapData bool) []string {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	body, ok := t.FieldByName("Body")
	if !ok {
		return nil
	}
	bt := derefType(body.Type)
	if unwrapData && bt.Kind() == reflect.Struct {
		// The D10 envelope carries the payload under json:"data"; drill into it.
		for i := 0; i < bt.NumField(); i++ {
			if jsonName(bt.Field(i)) == "data" {
				bt = derefType(bt.Field(i).Type)
				break
			}
		}
	}
	return jsonPropNames(bt)
}

// jsonPropNames returns the JSON property names of a struct type, honoring tags
// (json:"-" excluded), flattening embedded structs, sorted and de-duplicated.
func jsonPropNames(t reflect.Type) []string {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	seen := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := jsonName(f)
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			for _, n := range jsonPropNames(f.Type) {
				seen[n] = true
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// jsonName returns a struct field's JSON name from its tag: "-" when omitted, ""
// when there is no explicit name (untagged, or a bare `json:",omitempty"`).
func jsonName(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return ""
	}
	if tag == "-" {
		return "-"
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

// derefType unwraps pointer, slice, and array types to their element type.
func derefType(t reflect.Type) reflect.Type {
	for t != nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
		default:
			return t
		}
	}
	return t
}

func parseHandler(handlerSrc []byte) (*ast.File, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "handler.go", handlerSrc, 0)
	if err != nil {
		return nil, fmt.Errorf("resourcecheck: parse handler: %w", err)
	}
	return file, nil
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
