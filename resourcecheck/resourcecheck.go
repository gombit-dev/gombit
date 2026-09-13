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
// returns the model struct fields the create constructor actually persists — the
// keys of the `<typeName>{ ... }` composite literal RETURNED by the function
// named funcName (e.g. buildWidgetForCreate), which the handler passes straight
// to GORM's Create. Because only the returned value is inspected — not every
// literal syntactically present under the function — a decoy or temporary
// `<typeName>{...}` that is never returned does not falsely certify a field, and
// a literal in some other function is ignored. A hand-refactor that builds the
// returned value by other means (a named local, a helper) is treated
// conservatively: its fields look unassigned, surfacing as drift to resolve
// explicitly rather than a silent gap.
func ConstructorCreateFields(handlerSrc []byte, funcName, typeName string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handler.go", handlerSrc, 0)
	if err != nil {
		return nil, fmt.Errorf("resourcecheck: parse handler: %w", err)
	}
	set := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != funcName || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, res := range ret.Results {
				collectLiteralKeys(res, typeName, set)
			}
			return true
		})
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
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
