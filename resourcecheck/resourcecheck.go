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

	// The create method must be declared on the expected receiver (a same-named
	// method on another type is not the registered handler).
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

	// Exactly one persistence call `<recv>.….DB.….Create(&row)`: rooted at the
	// receiver AND passing through its .DB field, so `h.Audit.Create` or a bare
	// `.Create` on some other object cannot masquerade as the GORM write.
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
		if rootIdentName(sel.X) != recvVar || !chainSelects(sel.X, "DB") {
			return true
		}
		createCall = call
		createCount++
		return true
	})
	if createCount != 1 {
		return false, fmt.Sprintf("unsupported create-handler shape: expected exactly one %s.DB.….Create(&row) call, found %d", recvVar, createCount), nil
	}
	if enclosedInDisallowed(fn.Body, createCall) {
		return false, "unsupported create-handler shape: the Create call is inside a defer/go/loop/closure, so it is not the straight-line persistence", nil
	}

	createAddr := createCall.Args[0]
	createArg := identName(createAddr)
	createArgIdent := argIdent(createAddr)
	if createArg == "" || createArgIdent == nil {
		return false, "unsupported create-handler shape: the Create argument is not a plain &row variable", nil
	}

	// row is assigned exactly once, from ctorName, and never field-mutated or
	// incremented (server values belong inside the constructor literal).
	assignCount := 0
	var ctorAssign *ast.AssignStmt
	var ctorLHSIdent *ast.Ident
	mutated := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if mutated != "" {
			return false
		}
		switch s := n.(type) {
		case *ast.IncDecStmt:
			if touchesVar(s.X, createArg) {
				mutated = createArg + " is incremented/decremented"
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
						mutated = createArg + "." + target.Sel.Name + " is assigned after construction"
					}
				}
			}
		}
		return true
	})
	if mutated != "" {
		return false, "unsupported create-handler shape: " + mutated + "; set server-derived values inside " + ctorName, nil
	}
	if assignCount != 1 || ctorAssign == nil {
		return false, "unsupported create-handler shape: " + createArg + " must be assigned exactly once from " + ctorName + "(...)", nil
	}

	// Ordering + dominance: the constructor assignment and the Create call must
	// each be a TOP-LEVEL statement, assignment strictly first.
	ctorIdx, createIdx := -1, -1
	for i, stmt := range fn.Body.List {
		if stmt == ctorAssign {
			ctorIdx = i
		}
		if stmtReachesNode(stmt, createCall) {
			createIdx = i
		}
	}
	if ctorIdx < 0 || createIdx < 0 || ctorIdx >= createIdx {
		return false, "unsupported create-handler shape: " + ctorName + "(...) must be a top-level statement before the Create call", nil
	}

	// Allowlist every use of row before Create: only the constructor assignment's
	// LHS and the &row Create argument. Any other pre-Create use — a method call
	// (which may take &row implicitly), a stray &row, a read — is rejected. Uses
	// after Create (response mapping) are post-persist reads and are free.
	stray := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if stray != "" {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != createArg {
			return true
		}
		if id == ctorLHSIdent || id == createArgIdent || id.Pos() > createCall.End() {
			return true
		}
		stray = "unsupported create-handler shape: " + createArg + " is used before Create in a way that could mutate it (a method call, alias, or read)"
		return false
	})
	if stray != "" {
		return false, stray, nil
	}
	return true, "", nil
}

// validateConstructor requires ctorName to be a single unconditional
// `return <typeName>{...}` — no branches, locals, or alternate returns — so the
// value it produces is exactly the literal ConstructorCreateFields inspects.
func validateConstructor(file *ast.File, ctorName, typeName string) (bool, string) {
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if ok && d.Name != nil && d.Name.Name == ctorName && d.Body != nil {
			fn = d
			break
		}
	}
	if fn == nil {
		return false, "unsupported constructor shape: no " + ctorName + " function"
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

// chainSelects reports whether the selector/call chain rooted at expr passes
// through a `.field` selector (e.g. "DB" in `h.DB.WithContext(ctx)`).
func chainSelects(expr ast.Expr, field string) bool {
	for {
		switch e := expr.(type) {
		case *ast.SelectorExpr:
			if e.Sel != nil && e.Sel.Name == field {
				return true
			}
			expr = e.X
		case *ast.CallExpr:
			expr = e.Fun
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		default:
			return false
		}
	}
}

// enclosedInDisallowed reports whether target sits inside a defer, go, loop, or
// function literal within root — positions where lexical order is not runtime
// order, or the call may run zero or many times.
func enclosedInDisallowed(root, target ast.Node) bool {
	banned := false
	ast.Inspect(root, func(n ast.Node) bool {
		if banned {
			return false
		}
		switch n.(type) {
		case *ast.DeferStmt, *ast.GoStmt, *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
			if containsNode(n, target) {
				banned = true
				return false
			}
		}
		return true
	})
	return banned
}

// containsNode reports whether target appears anywhere within root.
func containsNode(root, target ast.Node) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		if found {
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
		case reflect.Ptr, reflect.Slice, reflect.Array:
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
