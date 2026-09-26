package resourcegen

import (
	"fmt"
	"reflect"
	"strings"
)

// This file is the model-first CRUD handler + hooks engine for ADR-016 (issue
// #352), slice 4. It consumes the modelResource that modelgen.go derives and
// emits two files per resource:
//
//   - a generator-owned handler (renderModelHandler): the Huma input/output
//     wrappers, a Handler over GORM, and the list/get/create operations. It is
//     DO-NOT-EDIT — regeneration owns it.
//   - a human-owned hooks file (renderModelHooks): a default no-op implementation
//     of the resource's hooks interface, generated ONCE and then owned by the
//     developer. This is where server-managed columns are set (ADR-016 moves them
//     off an opt-out list and onto an explicit BeforeCreate hook).
//
// The handler owns the invariant create sequence — build the model from the
// request DTO, run the BeforeCreate hook, persist — so a server-derived value
// (tenant, owner) enters through the hook, never by editing generated plumbing.
// The hooks interface is per-resource and typed (BeforeCreate takes *Model and
// the create body), so the seam is compiler-checked, not a map of callbacks.
//
// Slice 4 is the pure emitter: nothing calls it yet. Wiring it into
// `gombit make resource` (replacing the human-owned handler.go) and the
// `gombit generate --check` drift gate is slice 5.

// hooksInterfaceType / defaultHooksType name the per-resource hooks interface
// (BookHooks) and the human-owned concrete type (Hooks) that implements it. The
// interface is generator-owned; the concrete type lives in the human-owned file
// and is what Register wires in, so editing BeforeCreate is the supported
// customization path.
func (r modelResource) hooksInterfaceType() string { return r.TypeName + "Hooks" }
func (r modelResource) defaultHooksType() string   { return "Hooks" }

// renderModelHandler emits the generator-owned CRUD handler for one resource: the
// hooks interface, the Handler over GORM, the Huma input/output wrappers, the
// list/get/create operations, and Register. The DTOs and mappers it references
// (bookData, bookCreateBody, toBookData, bookFromCreateBody) come from the
// model-first DTO file in the same package (renderModelDTOs).
func renderModelHandler(r modelResource) (string, error) {
	name, err := parseResourceName(r.TypeName)
	if err != nil {
		return "", fmt.Errorf("resourcegen: derive resource names for %q: %w", r.TypeName, err)
	}
	pk, err := r.primaryKey()
	if err != nil {
		return "", err
	}
	uuidPK := pk.GoType == "uuid.UUID"

	typ := r.TypeName
	data := r.dataType()
	body := r.createBodyType()
	hooks := r.hooksInterfaceType()
	singular := strings.ToLower(typ)
	listOut := "list" + name.Tag + "Output"
	listIn := "list" + name.Tag + "Input"

	var b strings.Builder
	b.WriteString(goBanner())
	b.WriteString("package " + r.Package + "\n\n")
	stdImports := []string{"context", "net/http"}
	thirdImports := []string{
		"github.com/danielgtaylor/huma/v2",
		"github.com/gombit-dev/gombit/contract",
		"github.com/gombit-dev/gombit/database",
		"github.com/gombit-dev/gombit/framework",
	}
	if uuidPK {
		thirdImports = append(thirdImports, "github.com/google/uuid")
	} else {
		stdImports = append(stdImports, "strconv")
	}
	thirdImports = append(thirdImports, "gorm.io/gorm")
	b.WriteString(importBlock(stdImports, thirdImports))

	// Hooks interface: the resource's customization points. BeforeCreate receives
	// the model built from the request and the request body itself, so a hook can
	// set server-managed columns or derive values before the row is persisted.
	b.WriteString("// " + hooks + " are the customization points for the generated " + typ + " handler.\n")
	b.WriteString("// The generated handler owns the invariant CRUD sequence and calls these; set\n")
	b.WriteString("// server-managed columns (tenant, owner, …) in BeforeCreate rather than editing\n")
	b.WriteString("// generated plumbing.\n")
	b.WriteString("type " + hooks + " interface {\n")
	b.WriteString("\tBeforeCreate(ctx context.Context, row *" + typ + ", body " + body + ") error\n")
	b.WriteString("}\n\n")

	b.WriteString("// Handler serves " + typ + " HTTP operations over GORM. Register wires DB and Hooks.\n")
	b.WriteString("type Handler struct {\n")
	b.WriteString("\tDB    *gorm.DB\n")
	b.WriteString("\tHooks " + hooks + "\n")
	b.WriteString("}\n\n")

	// The declared list-query surface, derived from resolved policy facts (never by
	// re-reading tags here) — same wire semantics as the legacy generated handler.
	filters := r.filterFields()
	searchCols := r.searchColumns()
	sortCols := r.sortColumns()
	aggFields := r.aggregateFields()
	// Aggregatable fields return aggregates in meta (contract.ListMeta); without
	// them the plain contract.PageMeta keeps a no-query-surface resource identical.
	metaType := "contract.PageMeta"
	if len(aggFields) > 0 {
		metaType = "contract.ListMeta"
	}

	// Huma I/O wrappers (ADR-011: the request/response body nests under Body).
	b.WriteString("type " + listOut + " struct {\n")
	b.WriteString("\tBody contract.DataMeta[[]" + data + ", " + metaType + "]\n}\n\n")
	b.WriteString("type " + listIn + " struct {\n")
	b.WriteString("\tPage    int `query:\"page\" doc:\"1-based page\"`\n")
	b.WriteString("\tPerPage int `query:\"per_page\" doc:\"Page size\"`\n")
	if len(searchCols) > 0 {
		b.WriteString("\tSearch string `query:\"search\" doc:\"Search term matched across searchable fields\"`\n")
	}
	if len(sortCols) > 0 {
		b.WriteString("\tOrdering string `query:\"ordering\" doc:\"Field to order by; prefix with - for DESC (allowed: " + strings.Join(sortCols, ", ") + ")\"`\n")
	}
	if len(aggFields) > 0 {
		b.WriteString("\tAggregate string `query:\"aggregate\" doc:\"Comma-separated <func>:<field> aggregates over the filtered set, e.g. sum:" + aggFields[0].Column + " (funcs: sum, avg, min, max; fields: " + strings.Join(columnsOf(aggFields), ", ") + ")\"`\n")
	}
	for _, f := range filters {
		b.WriteString("\t" + f.GoName + " string `" + filterQueryTag(f) + "`\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("type get" + typ + "Input struct {\n")
	if uuidPK {
		b.WriteString("\tID string `path:\"id\" format:\"uuid\" doc:\"" + typ + " identifier\"`\n}\n\n")
	} else {
		b.WriteString("\tID string `path:\"id\" doc:\"" + typ + " identifier\"`\n}\n\n")
	}
	b.WriteString("type get" + typ + "Output struct {\n")
	b.WriteString("\tBody contract.Data[" + data + "]\n}\n\n")
	b.WriteString("type create" + typ + "Input struct {\n")
	b.WriteString("\tBody " + body + "\n}\n\n")
	b.WriteString("type create" + typ + "Output struct {\n")
	b.WriteString("\tBody contract.Data[" + data + "]\n}\n\n")

	// list: paginated read with the declared filter/search/sort/aggregate surface.
	// Filters and search narrow the set before the count, so meta.total reflects the
	// filtered collection; aggregates run over that same set before pagination.
	b.WriteString("func (h *Handler) list(ctx context.Context, input *" + listIn + ") (*" + listOut + ", error) {\n")
	b.WriteString("\tpage, perPage := contract.ClampPage(input.Page, input.PerPage)\n")
	b.WriteString("\tq := h.DB.WithContext(ctx).Model(&" + typ + "{})\n")
	errDeclared := false
	for _, f := range filters {
		assign := "="
		if !errDeclared {
			assign = ":="
			errDeclared = true
		}
		b.WriteString("\tq, err " + assign + " database.FilterEq(ctx, q, \"" + f.Column + "\", " + filterKindExpr(f) + ", input." + f.GoName + ")\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	}
	if len(searchCols) > 0 {
		b.WriteString("\tq = database.Search(q, []string{\"" + strings.Join(searchCols, "\", \"") + "\"}, input.Search)\n")
	}
	b.WriteString("\tvar total int64\n")
	b.WriteString("\tif err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {\n")
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.Internal(\"list " + name.PluralSnake + "\"))\n")
	b.WriteString("\t}\n")
	if len(aggFields) > 0 {
		b.WriteString("\taggs, err := database.ParseAggregates(ctx, input.Aggregate, map[string]database.AggregateColumn{\n")
		for _, f := range aggFields {
			b.WriteString("\t\t\"" + f.Column + "\": {Column: \"" + f.Column + "\"},\n")
		}
		b.WriteString("\t})\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		b.WriteString("\taggregates, err := database.Aggregate(ctx, q.Session(&gorm.Session{}), aggs)\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		errDeclared = true
	}
	if len(sortCols) > 0 {
		assign := "="
		if !errDeclared {
			assign = ":="
		}
		b.WriteString("\tq, err " + assign + " database.Ordering(ctx, q, input.Ordering, []string{\"" + strings.Join(sortCols, "\", \"") + "\"}, \"id\")\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	}
	b.WriteString("\tvar rows []" + typ + "\n")
	if len(sortCols) > 0 {
		b.WriteString("\tif err := q.Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(&rows).Error; err != nil {\n")
	} else {
		b.WriteString("\tif err := q.Order(\"id\").Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(&rows).Error; err != nil {\n")
	}
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.Internal(\"list " + name.PluralSnake + "\"))\n")
	b.WriteString("\t}\n")
	b.WriteString("\titems := make([]" + data + ", 0, len(rows))\n")
	b.WriteString("\tfor _, row := range rows {\n\t\titems = append(items, to" + typ + "Data(row))\n\t}\n")
	b.WriteString("\treturn &" + listOut + "{\n")
	b.WriteString("\t\tBody: contract.DataMeta[[]" + data + ", " + metaType + "]{\n")
	b.WriteString("\t\t\tData: items,\n")
	if len(aggFields) > 0 {
		b.WriteString("\t\t\tMeta: &" + metaType + "{Page: page, PerPage: perPage, Total: total, Aggregates: aggregates},\n")
	} else {
		b.WriteString("\t\t\tMeta: &" + metaType + "{Page: page, PerPage: perPage, Total: total},\n")
	}
	b.WriteString("\t\t},\n\t}, nil\n}\n\n")

	// get: load one by id.
	b.WriteString("func (h *Handler) get(ctx context.Context, input *get" + typ + "Input) (*get" + typ + "Output, error) {\n")
	if uuidPK {
		b.WriteString("\tid, err := uuid.Parse(input.ID)\n")
	} else {
		b.WriteString("\tid, err := strconv.ParseUint(input.ID, 10, 64)\n")
	}
	b.WriteString("\tif err != nil {\n")
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.NotFound(\"" + singular + " not found\"))\n")
	b.WriteString("\t}\n")
	b.WriteString("\tvar row " + typ + "\n")
	if uuidPK {
		b.WriteString("\tif err := h.DB.WithContext(ctx).First(&row, \"" + pk.Column + " = ?\", id).Error; err != nil {\n")
	} else {
		b.WriteString("\tif err := h.DB.WithContext(ctx).First(&row, uint(id)).Error; err != nil {\n")
	}
	b.WriteString("\t\treturn nil, database.MapLoadError(ctx, err, \"" + singular + " not found\", \"load " + singular + "\")\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn &get" + typ + "Output{Body: contract.Data[" + data + "]{Data: to" + typ + "Data(row)}}, nil\n}\n\n")

	// create: the invariant sequence — build from the request, run the hook, then
	// persist. bookFromCreateBody sets only request columns; server-managed columns
	// are the hook's responsibility, so they are never zero-filled silently. A nil
	// Hooks is a misconfigured Handler (Register always wires one), so fail closed
	// rather than skip the hook and persist a zero-filled server column — skipping
	// would silently reopen the exact zero-fill this design closes (#218).
	b.WriteString("func (h *Handler) create(ctx context.Context, input *create" + typ + "Input) (*create" + typ + "Output, error) {\n")
	b.WriteString("\tif h.Hooks == nil {\n")
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.Internal(\"create " + singular + ": Handler.Hooks is not set\"))\n")
	b.WriteString("\t}\n")
	b.WriteString("\trow := " + unexported(typ) + "FromCreateBody(input.Body)\n")
	b.WriteString("\tif err := h.Hooks.BeforeCreate(ctx, &row, input.Body); err != nil {\n")
	b.WriteString("\t\treturn nil, err\n")
	b.WriteString("\t}\n")
	b.WriteString("\tif err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {\n")
	b.WriteString("\t\treturn nil, database.MapPersistError(ctx, err, \"resource already exists\", \"create " + singular + "\")\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn &create" + typ + "Output{Body: contract.Data[" + data + "]{Data: to" + typ + "Data(row)}}, nil\n}\n\n")

	// Register mounts the routes and wires the human-owned Hooks. Gombit does not
	// discover feature packages by reflection; main calls this explicitly.
	b.WriteString("// Register mounts " + r.Package + " Huma routes, wiring the human-owned " + r.defaultHooksType() + ".\n")
	b.WriteString("func Register(app *framework.App) {\n")
	b.WriteString("\th := &Handler{DB: app.DB(), Hooks: " + r.defaultHooksType() + "{}}\n")
	b.WriteString("\tprefix := app.Config().API.Prefix\n")
	b.WriteString("\tapi := app.API()\n\n")
	// Operation IDs match the legacy path: list is plural (list-books), get/create
	// singular (get-book, create-book).
	writeHumaOp(&b, "list-"+name.Kebab, "http.MethodGet", "prefix + \""+name.HTTPPath+"\"", "List "+strings.ToLower(name.Tag), name.Tag, "h.list")
	writeHumaOp(&b, "get-"+name.Package, "http.MethodGet", "prefix + \""+name.HTTPPath+"/{id}\"", "Get a "+singular, name.Tag, "h.get")
	writeHumaOp(&b, "create-"+name.Package, "http.MethodPost", "prefix + \""+name.HTTPPath+"\"", "Create a "+singular, name.Tag, "h.create")
	b.WriteString("}\n")

	return b.String(), nil
}

// filterFields / aggregateFields return the resource's filterable / aggregatable
// columns in schema order (deterministic output). searchColumns / sortColumns
// return the DB column names for the searchable / sortable fields, in schema order.
func (r modelResource) primaryKey() (modelField, error) {
	var keys []modelField
	for _, f := range r.Fields {
		if f.PrimaryKey {
			keys = append(keys, f)
		}
	}
	if len(keys) == 0 {
		return modelField{}, fmt.Errorf("resourcegen: %s has no primary key", r.TypeName)
	}
	if len(keys) > 1 {
		return modelField{}, fmt.Errorf("resourcegen: %s has a composite primary key, which is not supported", r.TypeName)
	}
	switch keys[0].GoType {
	case "uint", "uuid.UUID":
		return keys[0], nil
	default:
		return modelField{}, fmt.Errorf("resourcegen: %s primary key type %s is not supported (supported: uint, uuid.UUID)", r.TypeName, keys[0].GoType)
	}
}

func (r modelResource) filterFields() []modelField {
	out := make([]modelField, 0, len(r.Fields))
	for _, f := range r.Fields {
		if f.Filterable {
			out = append(out, f)
		}
	}
	return out
}

func (r modelResource) aggregateFields() []modelField {
	out := make([]modelField, 0, len(r.Fields))
	for _, f := range r.Fields {
		if f.Aggregatable {
			out = append(out, f)
		}
	}
	return out
}

func (r modelResource) searchColumns() []string {
	var out []string
	for _, f := range r.Fields {
		if f.Searchable {
			out = append(out, f.Column)
		}
	}
	return out
}

func (r modelResource) sortColumns() []string {
	var out []string
	for _, f := range r.Fields {
		if f.Sortable {
			out = append(out, f.Column)
		}
	}
	return out
}

func columnsOf(fs []modelField) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Column
	}
	return out
}

// filterKindExpr maps the field's Go kind to the database.FilterKind the generated
// list handler passes to database.FilterEq to coerce the raw string query value —
// matching the legacy field-grammar mapping (int→FilterInt, int64→FilterInt64,
// unsigned→FilterUint, bool→FilterBool, otherwise string). uuid.UUID is an array
// of bytes, so it takes the string branch: char(36) stores the canonical text.
func filterKindExpr(f modelField) string {
	switch f.Kind {
	case reflect.Int:
		return "database.FilterInt"
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "database.FilterInt64"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "database.FilterUint"
	case reflect.Bool:
		return "database.FilterBool"
	default: // reflect.String
		return "database.FilterString"
	}
}

// filterQueryTag builds the Huma struct tag for a filter query param: the query
// name (the DB column), a true/false enum for a bool column so Huma rejects bad
// values, and a doc string. A string enum's allowed values live on the model's
// validate tag and are emitted on the create body; this filter param does not
// repeat them.
func filterQueryTag(f modelField) string {
	tag := `query:"` + f.Column + `"`
	if f.Kind == reflect.Bool {
		tag += ` enum:"true,false"`
	}
	tag += ` doc:"Filter by ` + f.GoName + ` (exact match)"`
	return tag
}

// writeHumaOp writes one huma.Register(...) call for an operation.
func writeHumaOp(b *strings.Builder, opID, method, path, summary, tag, handler string) {
	b.WriteString("\thuma.Register(api, huma.Operation{\n")
	b.WriteString("\t\tOperationID: \"" + opID + "\",\n")
	b.WriteString("\t\tMethod:      " + method + ",\n")
	b.WriteString("\t\tPath:        " + path + ",\n")
	b.WriteString("\t\tSummary:     \"" + summary + "\",\n")
	b.WriteString("\t\tTags:        []string{\"" + tag + "\"},\n")
	b.WriteString("\t}, " + handler + ")\n\n")
}

// renderModelHooks emits the human-owned hooks file: a default no-op
// implementation of the resource's hooks interface. Unlike the handler, this file
// carries NO DO-NOT-EDIT banner — it is generated once and then owned by the
// developer, who sets server-managed columns here. (Slice 5's generator writes it
// only when absent, never overwriting a customized copy.)
func renderModelHooks(r modelResource) string {
	typ := r.TypeName
	body := r.createBodyType()
	hooksType := r.defaultHooksType()

	var b strings.Builder
	b.WriteString("package " + r.Package + "\n\n")
	b.WriteString("import \"context\"\n\n")
	b.WriteString("// " + hooksType + " implements " + r.hooksInterfaceType() + ". This file is generated once and\n")
	b.WriteString("// is yours to edit: set server-managed columns (tenant, owner, timestamps not\n")
	b.WriteString("// handled by GORM, …) on row in BeforeCreate. Regeneration does not overwrite it.\n")
	b.WriteString("type " + hooksType + " struct{}\n\n")
	b.WriteString("// BeforeCreate runs after the request is mapped onto row and before it is\n")
	b.WriteString("// persisted. The default is a no-op; add server-derived values here.\n")
	b.WriteString("func (" + hooksType + ") BeforeCreate(ctx context.Context, row *" + typ + ", body " + body + ") error {\n")
	b.WriteString("\treturn nil\n}\n")
	return b.String()
}
