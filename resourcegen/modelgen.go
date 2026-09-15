package resourcegen

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/gombit-dev/gombit/resourcepolicy"
	"gorm.io/gorm/schema"
)

// This file is the model-first DTO/mapper engine for ADR-016 (issue #352). The
// legacy CLI path in fields.go/files.go derives DTOs from the `name:type` grammar
// and hands the developer a human-owned handler; the resulting request DTO,
// response DTO, and mappers freeze at generation time and drift from the model
// (issue #218). This engine instead derives them from the GORM model itself plus
// its declared resourcepolicy, so regeneration is the only synchronization step.
//
// Slice 3 is the pure emitter: given a parsed model it renders the request DTO,
// the response DTO, and the model↔DTO mappers as one generator-owned source file.
// Wiring it into `gombit make resource` (replacing the hand-owned handler) and
// the CRUD handler + hooks is slice 4; `gombit generate --check` is slice 5.

// modelResource is the model-derived shape the DTO/mapper emitter renders. It is
// built from the GORM schema and the resolved field policy — never from the CLI
// field grammar — so it always reflects the real model.
type modelResource struct {
	Package  string       // feature package the generated file belongs to, e.g. "book"
	TypeName string       // the model's exported Go type, e.g. "Book"
	Fields   []modelField // effective persisted columns, in schema order
	imports  []string     // sorted, de-duplicated import paths the DTOs reference
}

// modelField is one effective persisted column, projected for code generation.
// GoName is the flat DTO field identifier and struct-literal key; AccessPath is
// the model access expression (GORM's bind path joined by ".", so a promoted
// gorm.Model field is "Model.ID" and a named embed is "Audit.Note" — both
// compile, whether the embed is anonymous or named).
type modelField struct {
	GoName     string
	AccessPath string
	Column     string // DB column name; also the JSON field name
	GoType     string // rendered Go type expression, e.g. "string", "*time.Time", "types.Decimal"
	InRequest  bool
	InResponse bool
}

// buildModelResource parses a GORM model, resolves its field policy, and projects
// the result into a modelResource. It reads persistence facts (column, Go type,
// embedding path) from GORM's parsed schema and read/write/server policy from
// resourcepolicy — the two owners ADR-016 keeps separate. A model whose policy is
// contradictory or unsatisfiable fails closed here, as it does at resolve time.
func buildModelResource(pkg, typeName string, model any) (modelResource, error) {
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return modelResource{}, fmt.Errorf("resourcegen: parse model schema: %w", err)
	}
	policyFields, err := resourcepolicy.FromSchema(sch)
	if err != nil {
		return modelResource{}, err
	}
	resolved, err := resourcepolicy.ResolveAll(policyFields)
	if err != nil {
		return modelResource{}, err
	}

	res := modelResource{Package: pkg, TypeName: typeName}
	imports := map[string]struct{}{}
	pkgBaseByPath := map[string]string{}
	seenGoName := map[string]string{} // leaf name -> column that first claimed it

	for _, r := range resolved {
		if !r.InRequest && !r.InResponse {
			continue // hidden and hook-only columns never appear in a DTO
		}
		f := sch.FieldsByDBName[r.Column]
		if f == nil {
			// resolved columns come from sch.DBNames, so this cannot happen; guard
			// rather than emit a mapper referencing a column with no field.
			return modelResource{}, fmt.Errorf("resourcegen: resolved column %q has no schema field", r.Column)
		}
		// A flat DTO cannot carry two fields with the same Go name. Distinct
		// embedded structs with a same-named leaf (two "Note"s) would collide; fail
		// closed rather than emit a struct that will not compile.
		if prev, ok := seenGoName[r.GoName]; ok {
			return modelResource{}, fmt.Errorf("resourcegen: columns %q and %q both map to DTO field %q; rename one (embedded same-named fields are not yet supported)", prev, r.Column, r.GoName)
		}
		seenGoName[r.GoName] = r.Column

		goType, imps, err := renderGoType(f.FieldType)
		if err != nil {
			return modelResource{}, err
		}
		for _, imp := range imps {
			base := pkgBase(imp)
			if other, ok := pkgBaseByPath[base]; ok && other != imp {
				return modelResource{}, fmt.Errorf("resourcegen: imports %q and %q share package name %q; DTO generation cannot disambiguate them yet", other, imp, base)
			}
			pkgBaseByPath[base] = imp
			imports[imp] = struct{}{}
		}
		res.Fields = append(res.Fields, modelField{
			GoName:     r.GoName,
			AccessPath: strings.Join(f.BindNames, "."),
			Column:     r.Column,
			GoType:     goType,
			InRequest:  r.InRequest,
			InResponse: r.InResponse,
		})
	}

	res.imports = make([]string, 0, len(imports))
	for imp := range imports {
		res.imports = append(res.imports, imp)
	}
	sort.Strings(res.imports)
	return res, nil
}

// renderGoType renders a reflect.Type as a Go source type expression plus the
// import paths it needs. It covers the shapes a persisted GORM column takes: the
// predeclared scalars, named types (time.Time, database/sql null types, the
// framework decimal), pointers to any of these, and []byte blobs. It fails closed
// on a type it cannot render deterministically rather than emit code that will
// not compile.
func renderGoType(t reflect.Type) (expr string, imports []string, err error) {
	switch t.Kind() {
	case reflect.Pointer:
		inner, imps, err := renderGoType(t.Elem())
		if err != nil {
			return "", nil, err
		}
		return "*" + inner, imps, nil
	case reflect.Slice:
		if e := t.Elem(); e.Kind() == reflect.Uint8 && e.PkgPath() == "" {
			return "[]byte", nil, nil // the common blob column
		}
		inner, imps, err := renderGoType(t.Elem())
		if err != nil {
			return "", nil, err
		}
		return "[]" + inner, imps, nil
	}
	if pkgPath := t.PkgPath(); pkgPath != "" {
		name := t.Name()
		if name == "" {
			return "", nil, fmt.Errorf("resourcegen: cannot render unnamed type %s as a DTO field", t.String())
		}
		return pkgBase(pkgPath) + "." + name, []string{pkgPath}, nil
	}
	// A predeclared type (string, int, uint, bool, float64, ...) has no package.
	if t.Name() != "" {
		return t.Name(), nil, nil
	}
	return "", nil, fmt.Errorf("resourcegen: cannot render type %s as a DTO field", t.String())
}

// pkgBase is the trailing path segment of an import path, used to qualify a named
// type ("database/sql" -> "sql"). buildModelResource rejects two import paths that
// share a base, so this qualifier is unambiguous within a generated file.
func pkgBase(importPath string) string {
	if i := strings.LastIndexByte(importPath, '/'); i >= 0 {
		return importPath[i+1:]
	}
	return importPath
}

// requestFields / responseFields are the columns each DTO surfaces, in schema order.
func (r modelResource) requestFields() []modelField {
	out := make([]modelField, 0, len(r.Fields))
	for _, f := range r.Fields {
		if f.InRequest {
			out = append(out, f)
		}
	}
	return out
}

func (r modelResource) responseFields() []modelField {
	out := make([]modelField, 0, len(r.Fields))
	for _, f := range r.Fields {
		if f.InResponse {
			out = append(out, f)
		}
	}
	return out
}

// dataType / createBodyType name the response and request DTO Go types, following
// the existing convention (bookData) for the response and adding a distinct
// create-request type so the read/write split is explicit.
func (r modelResource) dataType() string       { return unexported(r.TypeName) + "Data" }
func (r modelResource) createBodyType() string { return unexported(r.TypeName) + "CreateBody" }

// renderModelDTOs emits the generator-owned DTO/mapper source for one resource:
// the response DTO, the create request DTO, and the model↔DTO mappers. Every
// field, its Go type, and its presence in each DTO is derived from the model and
// its policy, so the file cannot drift from the model without regeneration
// changing it (the mechanism gombit generate --check enforces in slice 5).
func renderModelDTOs(r modelResource) string {
	var b strings.Builder
	b.WriteString(goBanner())
	b.WriteString("package " + r.Package + "\n\n")

	var std, third []string
	for _, imp := range r.imports {
		if isStdImport(imp) {
			std = append(std, imp)
		} else {
			third = append(third, imp)
		}
	}
	b.WriteString(importBlock(std, third))

	data := r.dataType()
	body := r.createBodyType()
	typ := r.TypeName

	// Response DTO: the columns the API returns.
	b.WriteString("// " + data + " is the response body for a " + typ + ".\n")
	b.WriteString("type " + data + " struct {\n")
	for _, f := range r.responseFields() {
		b.WriteString("\t" + f.GoName + " " + f.GoType + " `json:\"" + f.Column + "\" doc:\"" + f.GoName + "\"`\n")
	}
	b.WriteString("}\n\n")

	// Create request DTO: the columns the client may set on create.
	b.WriteString("// " + body + " is the request body for creating a " + typ + ".\n")
	b.WriteString("type " + body + " struct {\n")
	for _, f := range r.requestFields() {
		b.WriteString("\t" + f.GoName + " " + f.GoType + " `json:\"" + f.Column + "\" doc:\"" + f.GoName + "\"`\n")
	}
	b.WriteString("}\n\n")

	// Response mapper: model -> response DTO. Composite-literal keys are the flat
	// DTO fields; values read the model through the bind path.
	b.WriteString("// to" + typ + "Data projects a " + typ + " into its response DTO.\n")
	b.WriteString("func to" + typ + "Data(row " + typ + ") " + data + " {\n")
	b.WriteString("\treturn " + data + "{\n")
	for _, f := range r.responseFields() {
		b.WriteString("\t\t" + f.GoName + ": row." + f.AccessPath + ",\n")
	}
	b.WriteString("\t}\n}\n\n")

	// Create mapper: request DTO -> a new model. Assignments (not a composite
	// literal) so a column reached through an embed (row.Audit.Note) is valid Go.
	// Only request columns are set here; server-managed columns are populated by
	// the create hook (slice 4), and DB/managed columns by GORM.
	b.WriteString("// " + unexported(typ) + "FromCreateBody builds a new " + typ + " from a create request.\n")
	b.WriteString("func " + unexported(typ) + "FromCreateBody(body " + body + ") " + typ + " {\n")
	b.WriteString("\tvar row " + typ + "\n")
	for _, f := range r.requestFields() {
		b.WriteString("\trow." + f.AccessPath + " = body." + f.GoName + "\n")
	}
	b.WriteString("\treturn row\n}\n")

	return b.String()
}

// isStdImport reports whether an import path is a standard-library package: its
// first path segment carries no dot (a domain), so "database/sql" is std and
// "github.com/gombit-dev/gombit/types" is not.
func isStdImport(importPath string) bool {
	first := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}
