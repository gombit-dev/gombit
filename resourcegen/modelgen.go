package resourcegen

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
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

// modelResource is the model-derived shape the DTO/mapper emitter renders. Its
// identity — the Go type name and the package the generated file belongs to — is
// derived from the parsed model itself (sch.ModelType), never supplied
// independently, so a modelResource cannot describe one model's fields under
// another type's name (ADR-016's one-authoritative-representation rule).
type modelResource struct {
	Package  string       // package clause; the model's own package (file sits beside the model)
	TypeName string       // the model's exported Go type, from sch.ModelType.Name()
	Fields   []modelField // effective persisted columns that appear in a DTO, in schema order
	imports  []importSpec // deterministic, explicit-aliased imports the DTO field types need
}

// modelField is one effective persisted column, projected for code generation.
// GoName is the flat DTO field identifier and struct-literal key; AccessPath is
// the model access expression (GORM's bind path joined by ".", e.g. "Model.ID" or
// "Audit.Note"). Only value embeds reach here — a column reached through a pointer
// embed is rejected at build time (buildModelResource), so AccessPath never
// dereferences a possibly-nil pointer.
type modelField struct {
	GoName     string
	AccessPath string
	Column     string // DB column name; also the JSON field name
	GoType     string // rendered Go type expression, e.g. "string", "*time.Time", "types.Decimal"
	InRequest  bool
	InResponse bool
}

// importSpec is one import the generated DTOs need, with the alias used to qualify
// its types. The alias is explicit for third-party packages because reflection
// exposes an import path, not the package's declared name — the two can differ
// (e.g. a package at .../foo.v2 declared `package foo`), and a bare import would
// bind the wrong identifier. Standard-library names always equal the path's last
// segment, so those stay bare.
type importSpec struct {
	Alias string
	Path  string
}

func (s importSpec) line() string {
	if isStdImport(s.Path) && s.Alias == pkgBase(s.Path) {
		return strconv.Quote(s.Path)
	}
	return s.Alias + " " + strconv.Quote(s.Path)
}

// buildModelResource parses a GORM model, resolves its field policy, and projects
// the result into a modelResource. Persistence facts (column, Go type, embedding
// path) come from GORM's parsed schema; read/write/server policy comes from
// resourcepolicy — the two owners ADR-016 keeps separate. Identity comes from the
// model's own type. It fails closed on a contradictory/unsatisfiable policy, a
// same-named leaf collision in the flat DTO, an anonymous model, a column reached
// through a pointer embed, and a field type it cannot render deterministically.
func buildModelResource(model any) (modelResource, error) {
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return modelResource{}, fmt.Errorf("resourcegen: parse model schema: %w", err)
	}
	typeName := sch.ModelType.Name()
	if typeName == "" {
		return modelResource{}, fmt.Errorf("resourcegen: cannot generate DTOs for an anonymous model type")
	}
	pkg := pkgBase(sch.ModelType.PkgPath())
	if !isGoIdent(pkg) {
		return modelResource{}, fmt.Errorf("resourcegen: model package %q (from %q) is not a usable Go identifier", pkg, sch.ModelType.PkgPath())
	}

	policyFields, err := resourcepolicy.FromSchema(sch)
	if err != nil {
		return modelResource{}, err
	}
	resolved, err := resourcepolicy.ResolveAll(policyFields)
	if err != nil {
		return modelResource{}, err
	}

	tr := newTypeRenderer(sch.ModelType.PkgPath(), pkg)
	res := modelResource{Package: pkg, TypeName: typeName}
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
		if err := ensureValueOnlyPath(sch.ModelType, f.BindNames, r.Column); err != nil {
			return modelResource{}, err
		}
		// A flat DTO cannot carry two fields with the same Go name. Distinct
		// embedded structs with a same-named leaf (two "Note"s) would collide; fail
		// closed rather than emit a struct that will not compile.
		if prev, ok := seenGoName[r.GoName]; ok {
			return modelResource{}, fmt.Errorf("resourcegen: columns %q and %q both map to DTO field %q; rename one (embedded same-named fields are not yet supported)", prev, r.Column, r.GoName)
		}
		seenGoName[r.GoName] = r.Column

		goType, err := tr.render(f.FieldType)
		if err != nil {
			return modelResource{}, err
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

	res.imports = tr.importSpecs()
	return res, nil
}

// ensureValueOnlyPath rejects a column reached through a pointer embed. GORM
// accepts an embedded *T (gorm:"embedded" on a pointer), and its bind path is
// indistinguishable from a value embed's — but the generated create mapper
// zero-initializes the model and then assigns through the path, dereferencing a
// nil pointer. Rather than emit a mapper that panics, walk the model type along
// the bind path and fail closed if any intermediate field is a pointer. (Value
// embeds, including gorm.Model, are fine.)
func ensureValueOnlyPath(modelType reflect.Type, bindNames []string, column string) error {
	t := modelType
	for _, seg := range bindNames[:len(bindNames)-1] { // the leaf is the column itself
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		sf, ok := t.FieldByName(seg)
		if !ok {
			return fmt.Errorf("resourcegen: cannot resolve embed segment %q for column %q", seg, column)
		}
		if sf.Type.Kind() == reflect.Pointer {
			return fmt.Errorf("resourcegen: column %q is reached through a pointer embed (%s *%s); pointer embeds are not supported yet", column, seg, sf.Type.Elem().Name())
		}
		t = sf.Type
	}
	return nil
}

// typeRenderer renders reflect.Type field types as Go source expressions and
// tracks the imports they need. It renders a type local to the model's package
// unqualified (the generated file lives in that package), a predeclared type by
// its name, and any other named type through a deterministic explicit alias.
type typeRenderer struct {
	modelPkgPath string
	reserved     string            // the output package name; never reuse as an import alias
	aliasByPath  map[string]string // import path -> alias, assigned on first use
	aliasTaken   map[string]bool
}

func newTypeRenderer(modelPkgPath, outputPkg string) *typeRenderer {
	return &typeRenderer{
		modelPkgPath: modelPkgPath,
		reserved:     outputPkg,
		aliasByPath:  map[string]string{},
		aliasTaken:   map[string]bool{},
	}
}

// render returns the Go source type expression for t, recording any import it
// needs. It fails closed on a type it cannot render deterministically (an unnamed
// composite other than a pointer or slice, or a package whose alias cannot be
// derived) rather than emit code that will not compile.
func (tr *typeRenderer) render(t reflect.Type) (string, error) {
	// Unnamed types (pointers, and slices like []byte / []string) carry no package
	// and are decomposed structurally. A named type — even a defined slice/map —
	// has a package path and is rendered by identity in the branch below, so it
	// keeps its own type (and its JSON/DB behavior).
	if t.PkgPath() == "" {
		switch t.Kind() {
		case reflect.Pointer:
			inner, err := tr.render(t.Elem())
			if err != nil {
				return "", err
			}
			return "*" + inner, nil
		case reflect.Slice:
			if e := t.Elem(); e.Kind() == reflect.Uint8 && e.PkgPath() == "" {
				return "[]byte", nil // the common blob column
			}
			inner, err := tr.render(t.Elem())
			if err != nil {
				return "", err
			}
			return "[]" + inner, nil
		}
		if t.Name() != "" { // a predeclared type: string, int, uint, bool, float64, ...
			return t.Name(), nil
		}
		return "", fmt.Errorf("resourcegen: cannot render type %s as a DTO field", t.String())
	}

	name := t.Name()
	if t.PkgPath() == tr.modelPkgPath {
		return name, nil // local to the model's package: unqualified, no import
	}
	alias, err := tr.aliasFor(t.PkgPath())
	if err != nil {
		return "", err
	}
	return alias + "." + name, nil
}

// aliasFor returns the import alias for a path, assigning one deterministically on
// first use: the path's last segment when that is a Go identifier, else a
// synthetic name, uniquified against aliases already assigned and the output
// package name. Assignment order follows first field-encounter order, so output
// is deterministic.
func (tr *typeRenderer) aliasFor(path string) (string, error) {
	if a, ok := tr.aliasByPath[path]; ok {
		return a, nil
	}
	base := pkgBase(path)
	if !isGoIdent(base) {
		base = "pkg"
	}
	alias := base
	for i := 1; tr.aliasTaken[alias] || alias == tr.reserved; i++ {
		alias = base + strconv.Itoa(i)
	}
	tr.aliasByPath[path] = alias
	tr.aliasTaken[alias] = true
	return alias, nil
}

// importSpecs returns the assigned imports, sorted by path for stable output.
func (tr *typeRenderer) importSpecs() []importSpec {
	out := make([]importSpec, 0, len(tr.aliasByPath))
	for path, alias := range tr.aliasByPath {
		out = append(out, importSpec{Alias: alias, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// pkgBase is the trailing path segment of an import path ("database/sql" -> "sql").
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
	b.WriteString(renderImports(r.imports))

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
	// literal) so a column reached through a value embed (row.Audit.Note) is valid
	// Go. Only request columns are set here; server-managed columns are populated
	// by the create hook (slice 4), and DB/managed columns by GORM.
	b.WriteString("// " + unexported(typ) + "FromCreateBody builds a new " + typ + " from a create request.\n")
	b.WriteString("func " + unexported(typ) + "FromCreateBody(body " + body + ") " + typ + " {\n")
	b.WriteString("\tvar row " + typ + "\n")
	for _, f := range r.requestFields() {
		b.WriteString("\trow." + f.AccessPath + " = body." + f.GoName + "\n")
	}
	b.WriteString("\treturn row\n}\n")

	return b.String()
}

// renderImports renders the import declaration for the DTO field types: standard
// library first, then third-party, each sorted by path, third-party carrying
// explicit aliases (see importSpec). Empty groups are omitted; a lone import
// keeps the single-line form.
func renderImports(specs []importSpec) string {
	if len(specs) == 0 {
		return ""
	}
	var std, third []string
	for _, s := range specs {
		if isStdImport(s.Path) {
			std = append(std, "\t"+s.line())
		} else {
			third = append(third, "\t"+s.line())
		}
	}
	if len(specs) == 1 {
		return "import " + specs[0].line() + "\n\n"
	}
	var groups []string
	if len(std) > 0 {
		groups = append(groups, strings.Join(std, "\n"))
	}
	if len(third) > 0 {
		groups = append(groups, strings.Join(third, "\n"))
	}
	return "import (\n" + strings.Join(groups, "\n\n") + "\n)\n\n"
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
