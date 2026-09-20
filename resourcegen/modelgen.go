package resourcegen

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gombit-dev/gombit/resourcepolicy"
	"github.com/gombit-dev/gombit/types"
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

// modelResource is the model-derived shape the DTO/mapper emitter renders. The
// Go type name is derived from the parsed model itself (sch.ModelType.Name()),
// never supplied independently, so a modelResource cannot describe one model's
// fields under another type's name (ADR-016's one-authoritative-representation
// rule). The package clause CANNOT be derived that way: reflect.Type.PkgPath
// returns an import path, not the package's declared name, and the two can
// differ (a package at .../foo.v2 declaring `package foo`; a directory name
// that does not match its package clause). buildModelResource instead takes
// the destination package as an explicit, validated parameter — the caller
// (slice 4) knows it, because it is the package the file is about to be
// written into beside model.go.
type modelResource struct {
	Package  string       // package clause; validated identifier, supplied by the caller
	TypeName string       // the model's exported Go type, from sch.ModelType.Name()
	Fields   []modelField // effective persisted columns that appear in a DTO, in schema order
	imports  []importSpec // deterministic, explicit-aliased imports the DTO field types need
}

// modelField is one effective persisted column, projected for code generation.
// It embeds resourcepolicy.Resolved rather than re-picking a few of its fields:
// the projection ADDS generator-specific facts (AccessPath, GoType) to the
// authoritative policy facts, it does not SUBTRACT from them. Slice 4 needs
// CreateSource to generate the create-hook signature (which columns the hook
// must set) and NotNull/HasDefault/PrimaryKey to generate the same validation
// and migration decisions the legacy path made from its own field grammar —
// discarding those here would force slice 4 to re-parse the model and
// re-resolve the policy from scratch, the exact duplicated derivation ADR-016
// exists to abolish (issue #352 review).
//
// AccessPath is the model access expression (GORM's bind path joined by ".",
// e.g. "Model.ID" or "Audit.Note"). Only value embeds reach here — a column
// reached through a pointer embed is rejected at build time
// (buildModelResource), so AccessPath never dereferences a possibly-nil
// pointer.
type modelField struct {
	resourcepolicy.Resolved
	AccessPath string
	GoType     string // rendered Go type expression, e.g. "string", "*time.Time", "types.Decimal"
	// Size is the column's declared size (GORM schema.Field.Size), used to derive a
	// maxLength on a string create field. It is emitted only when > 0 (a real
	// varchar(N)); an unset/driver-dependent size (0 or -1) yields no maxLength —
	// we only assert a constraint the model unambiguously states.
	Size int
	// Kind is the field's EFFECTIVE scalar reflect.Kind, with nullability unwrapped
	// (a *string / sql.NullString column reports reflect.String) — see
	// effectiveKind. It is a type CLASSIFICATION, distinct from GoType's rendered
	// TEXT: a defined type `type Slug string` renders as "Slug" (by identity, on
	// purpose) but has Kind reflect.String. Kind-based decisions (minLength for a
	// string column, whether a column can be filtered/sorted/aggregated) must key
	// off this, never off the GoType string or the raw (possibly pointer) kind.
	Kind reflect.Kind
}

// importSpec is one import the generated DTOs need, with the alias used to qualify
// its types. Every import is written WITH its explicit alias, never bare: a bare
// import binds the package's declared name, which reflection cannot see — it
// exposes only the import path, whose last segment need not equal the declared
// name (a package at .../foo.v2 declared `package foo`; an internal package whose
// folder name differs from its `package` clause, legal under a non-domain module
// path like `gombit new --module myapp`). The explicit alias makes the qualifier
// used in the body (alias.Type) correct regardless. See aliasFor for how it is
// chosen deterministically.
type importSpec struct {
	Alias string
	Path  string
}

func (s importSpec) line() string {
	return s.Alias + " " + strconv.Quote(s.Path)
}

// buildModelResource parses a GORM model, resolves its field policy, and projects
// the result into a modelResource. Persistence facts (column, Go type, embedding
// path) come from GORM's parsed schema; read/write/server policy comes from
// resourcepolicy — the two owners ADR-016 keeps separate. The type name comes
// from the model's own type; the destination package comes from pkg, which the
// caller must supply — see modelResource's doc comment for why it cannot be
// derived from reflection. It fails closed on a contradictory/unsatisfiable
// policy, an unusable destination package, a same-named leaf collision in the
// flat DTO, an anonymous model, a column reached through a pointer embed, and a
// field type it cannot render deterministically.
//
// pkg is validated as a usable Go identifier here (the bar every caller can
// check). Cross-checking it against the package clause already on disk beside
// model.go — the stronger guarantee once a real destination file exists — is
// slice 4's job: this pure emitter never touches a filesystem.
func buildModelResource(model any, pkg string) (modelResource, error) {
	if !isUsableIdent(pkg) {
		return modelResource{}, fmt.Errorf("resourcegen: destination package %q is not a usable Go identifier", pkg)
	}
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return modelResource{}, fmt.Errorf("resourcegen: parse model schema: %w", err)
	}
	typeName := sch.ModelType.Name()
	if typeName == "" {
		return modelResource{}, fmt.Errorf("resourcegen: cannot generate DTOs for an anonymous model type")
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
		mf := modelField{
			Resolved:   r,
			AccessPath: strings.Join(f.BindNames, "."),
			GoType:     goType,
			Kind:       effectiveKind(f.FieldType),
			Size:       f.Size,
		}
		// resourcepolicy validated the query capabilities as API policy (declared,
		// response-visible); here, where the Go type is known, validate that the
		// column's type can actually support the operation — the split the legacy
		// path made from its field grammar, kept type-appropriate.
		if err := validateQueryTypes(mf, f.FieldType); err != nil {
			return modelResource{}, err
		}
		res.Fields = append(res.Fields, mf)
	}

	res.imports = tr.importSpecs()
	return res, nil
}

// decimalType is the framework decimal, the one non-primitive numeric type that
// is aggregatable.
var decimalType = reflect.TypeOf(types.Decimal{})

// sqlNullKinds maps the database/sql nullable wrappers to the scalar kind they
// carry, so a nullable column is classified by its underlying type — not by the
// struct wrapper — for query-capability decisions.
var sqlNullKinds = map[reflect.Type]reflect.Kind{
	reflect.TypeOf(sql.NullString{}):  reflect.String,
	reflect.TypeOf(sql.NullInt64{}):   reflect.Int64,
	reflect.TypeOf(sql.NullInt32{}):   reflect.Int32,
	reflect.TypeOf(sql.NullInt16{}):   reflect.Int16,
	reflect.TypeOf(sql.NullByte{}):    reflect.Uint8,
	reflect.TypeOf(sql.NullBool{}):    reflect.Bool,
	reflect.TypeOf(sql.NullFloat64{}): reflect.Float64,
}

// effectiveKind is the field's scalar kind for query-capability decisions with
// nullability unwrapped once: a pointer column (*string, *int64) reports the kind
// it points to, and a database/sql wrapper (sql.NullString, …) the kind it
// carries. Nullability is orthogonal to whether a column can be filtered / sorted
// / searched / aggregated (the legacy field grammar draws no such distinction),
// so every query check must consult this, never the raw reflect.Kind.
func effectiveKind(t reflect.Type) reflect.Kind {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if k, ok := sqlNullKinds[t]; ok {
		return k
	}
	return t.Kind()
}

// validateQueryTypes fails closed when a column declares a query capability its Go
// type cannot support, matching the legacy field-grammar type rules: filterable ⊂
// {string, integer, bool}; searchable ⊂ {string}; aggregatable ⊂ numeric (integer,
// float, or the framework decimal). Sortable has no type restriction — any
// persisted column can be ordered.
func validateQueryTypes(f modelField, ft reflect.Type) error {
	if f.Filterable && !isFilterableKind(f.Kind) {
		return fmt.Errorf("resourcegen: column %q (%s) is not filterable; a filterable column must be a string, integer, or bool", f.Column, f.GoType)
	}
	if f.Searchable && f.Kind != reflect.String {
		return fmt.Errorf("resourcegen: column %q (%s) is not searchable; a searchable column must be a string", f.Column, f.GoType)
	}
	if f.Aggregatable && !isAggregatableType(f.Kind, ft) {
		return fmt.Errorf("resourcegen: column %q (%s) is not aggregatable; an aggregatable column must be numeric (integer, float, or decimal)", f.Column, f.GoType)
	}
	return nil
}

func isFilterableKind(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

func isAggregatableType(k reflect.Kind, ft reflect.Type) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	return ft == decimalType
}

// ensureValueOnlyPath rejects a column reached through a pointer embed. GORM
// accepts an embedded *T (gorm:"embedded" on a pointer), and its bind path is
// indistinguishable from a value embed's — but the generated create mapper
// zero-initializes the model and then assigns through the path, dereferencing a
// nil pointer. Rather than emit a mapper that panics, walk the model type along
// the bind path and fail closed if any intermediate field is a pointer. (Value
// embeds, including gorm.Model, are fine.)
//
// Invariant this relies on: bindNames is the LITERAL, level-by-level chain GORM
// walked to reach the column — never a promoted shortcut — so at each step the
// segment names a field declared directly on the current level. That is why the
// lookup below scans t's own fields instead of calling reflect.Type.FieldByName:
// FieldByName does a promotion-aware breadth-first search through embedded
// fields (including embedded pointers) and can return a field of the same name
// promoted from *deeper* in the type, whose Type is the leaf's — not the pointer
// actually traversed at this level, which is exactly the fact this function
// exists to catch (issue #352 review).
func ensureValueOnlyPath(modelType reflect.Type, bindNames []string, column string) error {
	// modelType is sch.ModelType, already dereferenced by schema.Parse, and every
	// non-leaf field walked below has just been confirmed non-pointer by the
	// previous iteration's own check — so t is a struct type at every step, never
	// a pointer needing another deref here.
	t := modelType
	for _, seg := range bindNames[:len(bindNames)-1] { // the leaf is the column itself
		sf, ok := fieldDeclaredAt(t, seg)
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

// fieldDeclaredAt returns the struct field named seg declared directly on t —
// depth 0 only, never a field promoted from an embedded type. Unlike
// reflect.Type.FieldByName (a promotion-aware breadth-first search), this
// answers exactly "is there a field literally named seg at this level",
// which is what a bind-path walk needs at each step (see ensureValueOnlyPath).
func fieldDeclaredAt(t reflect.Type, seg string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.Name == seg {
			return f, true
		}
	}
	return reflect.StructField{}, false
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
// needs. It fails closed on a type it cannot render deterministically (an
// unnamed composite other than a pointer or slice, or a non-local named type
// that is unexported) rather than emit code that will not compile.
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
	// Outside the model's package, name is about to be qualified as alias.name —
	// which only refers to something if name is exported. An unexported name here
	// (reachable through a type alias to an unexported type in a dependency,
	// `type Exported = unexported`) would render a reference no other package can
	// compile against. name == "" (an anonymous type with a non-empty PkgPath
	// cannot normally occur, but nothing guarantees it) is rejected the same way.
	if !isExportedIdent(name) {
		return "", fmt.Errorf("resourcegen: type %s.%s is unexported and cannot be referenced from outside its package", t.PkgPath(), name)
	}
	return tr.aliasFor(t.PkgPath()) + "." + name, nil
}

// aliasFor returns the import alias for a path, assigning one deterministically on
// first use: the path's last segment when that is a usable Go identifier (neither
// empty nor a keyword — "map", "type", "select" are real package-path segments),
// else a synthetic name, uniquified against aliases already assigned and the
// output package name. Assignment order follows first field-encounter order, so
// output is deterministic.
func (tr *typeRenderer) aliasFor(path string) string {
	if a, ok := tr.aliasByPath[path]; ok {
		return a
	}
	base := pkgBase(path)
	if !isUsableIdent(base) {
		base = "pkg"
	}
	alias := base
	for i := 1; tr.aliasTaken[alias] || alias == tr.reserved; i++ {
		alias = base + strconv.Itoa(i)
	}
	tr.aliasByPath[path] = alias
	tr.aliasTaken[alias] = true
	return alias
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

// jsonName is the wire field name: derived from the Go field name, the same way
// the legacy field-grammar path derives it (toSnake, fields.go). It is
// deliberately NOT f.Column: Column is a persistence decision (ADR-016 puts it
// under "GORM schema owns"), and a `gorm:"column:..."` override — e.g. legacy
// database naming inside an embedded struct — must not silently become part of
// the wire contract without that being an explicit, separate choice (issue #352
// review).
func (f modelField) jsonName() string { return toSnake(f.GoName) }

// responseTag is the struct tag for f in the response DTO: wire name plus doc.
func (f modelField) responseTag() string {
	return `json:"` + f.jsonName() + `" doc:"` + f.GoName + `"`
}

// requestTag is the struct tag for f in the create request DTO: the wire name,
// the create-input validation the schema unambiguously implies, and doc. All
// constraints are schema-derived (never re-parsed from the CLI grammar):
//
//	minLength:"1"    a NOT NULL string — else "" satisfies resourcepolicy (a create
//	                 source exists) but is not the data the column requires (#218).
//	maxLength:"<N>"  a string whose column has a real size (Size > 0); an
//	                 unset/driver-dependent size asserts nothing.
//	minimum:"0"      an unsigned integer column.
//
// The string checks key off f.Kind (reflect.String), not the GoType text: a
// defined `type Slug string` renders as "Slug" yet is Kind reflect.String and
// still zero-fills to "", so it needs the same constraint. Enum value constraints
// are NOT emitted — the GORM schema stores an enum as a plain varchar, so the
// allowed values are not a recoverable schema fact (a known class-B gap).
func (f modelField) requestTag() string {
	tag := `json:"` + f.jsonName() + `"`
	if f.Kind == reflect.String {
		if f.NotNull {
			tag += ` minLength:"1"`
		}
		if f.Size > 0 {
			tag += ` maxLength:"` + strconv.Itoa(f.Size) + `"`
		}
	}
	if isUnsignedKind(f.Kind) {
		tag += ` minimum:"0"`
	}
	tag += ` doc:"` + f.GoName + `"`
	return tag
}

// isUnsignedKind reports whether the (unwrapped) kind is an unsigned integer, so
// the create field carries minimum:"0" — GORM stores unsigned columns as
// non-negative, matching the legacy generator.
func isUnsignedKind(k reflect.Kind) bool {
	switch k {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

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
		b.WriteString("\t" + f.GoName + " " + f.GoType + " `" + f.responseTag() + "`\n")
	}
	b.WriteString("}\n\n")

	// Create request DTO: the columns the client may set on create.
	b.WriteString("// " + body + " is the request body for creating a " + typ + ".\n")
	b.WriteString("type " + body + " struct {\n")
	for _, f := range r.requestFields() {
		b.WriteString("\t" + f.GoName + " " + f.GoType + " `" + f.requestTag() + "`\n")
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

// renderImports renders the import declaration for the DTO field types: one block,
// every import explicitly aliased (see importSpec), sorted by path (importSpecs
// already sorts, and gofmt orders an import block by path, so the two agree and
// output is stable). No standard-library-vs-third-party split: deciding that from
// an import path is the very heuristic importSpec avoids, and grouping is cosmetic
// once every import is aliased. A lone import keeps the single-line form.
func renderImports(specs []importSpec) string {
	if len(specs) == 0 {
		return ""
	}
	if len(specs) == 1 {
		return "import " + specs[0].line() + "\n\n"
	}
	lines := make([]string, 0, len(specs))
	for _, s := range specs {
		lines = append(lines, "\t"+s.line())
	}
	return "import (\n" + strings.Join(lines, "\n") + "\n)\n\n"
}
