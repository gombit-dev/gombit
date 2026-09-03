package resourcegen

import (
	"fmt"
	"go/format"
	"sort"
	"strings"
)

type fileSpec struct {
	relPath string
	content []byte
	owned   bool // AST-edited user file; additive, not banner-gated
}

type renderContext struct {
	Resource   ResourceName
	Fields     []Field
	Module     string
	ImportPath string
	ModelSpec  string
	APIPrefix  string
	UI         string
	Service    bool
	Repo       bool
	DataType   string
}

func newRenderContext(module string, name ResourceName, fields []Field, apiPrefix, ui string, service, repo bool) renderContext {
	if ui == "" {
		ui = defaultUI
	}
	return renderContext{
		Resource:   name,
		Fields:     fields,
		Module:     module,
		ImportPath: module + "/internal/" + name.Package,
		ModelSpec:  module + "/internal/" + name.Package + "." + name.TypeName,
		APIPrefix:  apiPrefix,
		UI:         ui,
		Service:    service,
		Repo:       repo,
		DataType:   unexported(name.TypeName) + "Data",
	}
}

func unexported(name string) string {
	if name == "" {
		return name
	}
	return strings.ToLower(name[:1]) + name[1:]
}

func renderFeatureFiles(ctx renderContext) ([]fileSpec, error) {
	files := []fileSpec{
		{relPath: fmt.Sprintf("internal/%s/%s.go", ctx.Resource.Package, ctx.Resource.FileBase), content: mustFormatGo(renderModel(ctx))},
		{relPath: fmt.Sprintf("internal/%s/handler.go", ctx.Resource.Package), content: mustFormatGo(renderHandler(ctx))},
		{relPath: fmt.Sprintf("internal/%s/routes.go", ctx.Resource.Package), content: mustFormatGo(renderRoutes(ctx))},
	}
	if ctx.Service {
		files = append(files, fileSpec{
			relPath: fmt.Sprintf("internal/%s/service.go", ctx.Resource.Package),
			content: mustFormatGo(renderService(ctx)),
		})
	}
	if ctx.Repo {
		files = append(files, fileSpec{
			relPath: fmt.Sprintf("internal/%s/repo.go", ctx.Resource.Package),
			content: mustFormatGo(renderRepo(ctx)),
		})
	}

	// The generated frontend (thin CRUD) sees only the DTO fields: belongs_to as
	// its uint FK, m2m / has_many dropped (edited through the admin).
	tsxCtx := ctx
	tsxCtx.Fields = dtoFields(ctx.Fields)
	files = append(files,
		fileSpec{relPath: fmt.Sprintf("frontend/src/%s/list.tsx", ctx.Resource.Package), content: []byte(renderListTSX(tsxCtx))},
		fileSpec{relPath: fmt.Sprintf("frontend/src/%s/form.tsx", ctx.Resource.Package), content: []byte(renderFormTSX(tsxCtx))},
	)
	return files, nil
}

func mustFormatGo(src string) []byte {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		// Leave unformatted source so tests can show the parse error.
		return []byte(src + "\n// format error: " + err.Error() + "\n")
	}
	return formatted
}

func goBanner() string {
	return "// " + GeneratedBanner + "\n"
}

func tsBanner() string {
	return "/**\n * " + GeneratedBanner + "\n */\n"
}

// fieldsUse reports whether any field has the given type.
func fieldsUse(fields []Field, t FieldType) bool {
	for _, f := range fields {
		if f.Type == t {
			return true
		}
	}
	return false
}

// importBlock renders a grouped Go import block: standard library first, a
// blank line, then third-party. Empty groups are omitted. A lone import keeps
// the idiomatic single-line form.
func importBlock(std, third []string) string {
	if len(std)+len(third) == 1 {
		only := append(append([]string{}, std...), third...)[0]
		return "import \"" + only + "\"\n\n"
	}
	var groups []string
	if len(std) > 0 {
		groups = append(groups, strings.Join(quoteImports(std), "\n"))
	}
	if len(third) > 0 {
		groups = append(groups, strings.Join(quoteImports(third), "\n"))
	}
	if len(groups) == 0 {
		return ""
	}
	return "import (\n" + strings.Join(groups, "\n\n") + "\n)\n\n"
}

func quoteImports(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, "\t\""+p+"\"")
	}
	return out
}

// gombitTypesImport is the framework value-types package used for decimal.
const gombitTypesImport = "github.com/gombit-dev/gombit/types"

func renderModel(ctx renderContext) string {
	var b strings.Builder
	b.WriteString(goBanner())
	b.WriteString("package ")
	b.WriteString(ctx.Resource.Package)
	b.WriteString("\n\n")

	var std, third []string
	if fieldsUse(ctx.Fields, FieldTime) {
		std = append(std, "time")
	}
	third = append(third, "gorm.io/gorm")
	if fieldsUse(ctx.Fields, FieldDecimal) {
		third = append(third, gombitTypesImport)
	}
	third = append(third, targetImports(ctx)...)
	b.WriteString(importBlock(std, third))
	b.WriteString("// ")
	b.WriteString(ctx.Resource.TypeName)
	b.WriteString(" is the feature-package GORM model.\n")
	b.WriteString("type ")
	b.WriteString(ctx.Resource.TypeName)
	b.WriteString(" struct {\n\tgorm.Model\n")
	for _, field := range ctx.Fields {
		b.WriteString(modelFieldLines(field, ctx.Resource.Package))
	}
	b.WriteString("}\n")
	return b.String()
}

// targetImports returns the distinct feature-package import paths for the
// relation fields' target models (internal/<target>).
func targetImports(ctx renderContext) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, f := range ctx.Fields {
		if !f.isRelation() {
			continue
		}
		imp := ctx.Module + "/internal/" + f.TargetPkg
		if _, ok := seen[imp]; ok {
			continue
		}
		seen[imp] = struct{}{}
		out = append(out, imp)
	}
	sort.Strings(out)
	return out
}

// modelFieldLines renders the struct field line(s) for one model field. A
// belongs_to emits the foreign key plus the association; has_many / m2m emit the
// association slice (m2m carries the join-table tag).
func modelFieldLines(f Field, resourcePkg string) string {
	switch f.Type {
	case FieldBelongsTo:
		return "\t" + f.fkGoName() + " uint `gorm:\"index\"`\n" +
			"\t" + f.GoName + " " + f.GoType + "\n"
	case FieldHasMany:
		return "\t" + f.GoName + " " + f.GoType + "\n"
	case FieldManyToMany:
		return "\t" + f.GoName + " " + f.GoType + " `gorm:\"many2many:" + f.joinTable(resourcePkg) + ";\"`\n"
	default:
		line := "\t" + f.GoName + " " + f.GoType
		if tag := f.gormTag(); tag != "" {
			line += " `gorm:\"" + tag + "\"`"
		}
		return line + "\n"
	}
}

// filterFields returns the fields the generated list handler exposes as
// exact-match query filters, in declared order (belongs_to FKs are included by
// default; see Field.isFilterable).
func filterFields(fields []Field) []Field {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		if f.isFilterable() {
			out = append(out, f)
		}
	}
	return out
}

// searchColumns returns the DB columns ?search= searches, in declared order.
func searchColumns(fields []Field) []string {
	var out []string
	for _, f := range fields {
		if f.Searchable {
			out = append(out, f.searchColumn())
		}
	}
	return out
}

// sortColumns returns the DB columns ?ordering= may order by, in declared order.
func sortColumns(fields []Field) []string {
	var out []string
	for _, f := range fields {
		if f.Sortable {
			out = append(out, f.sortColumn())
		}
	}
	return out
}

// aggregateFields returns the fields ?aggregate= may compute SUM/AVG/MIN/MAX
// over, in declared order (issue #272).
func aggregateFields(fields []Field) []Field {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		if f.Aggregatable {
			out = append(out, f)
		}
	}
	return out
}

// aggregateColumns returns the DB column names for the given aggregatable
// fields, in order — used for the ?aggregate= param doc string.
func aggregateColumns(fields []Field) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.aggregateColumn())
	}
	return out
}

func renderHandler(ctx renderContext) string {
	var b strings.Builder
	pkg := ctx.Resource.Package
	typ := ctx.Resource.TypeName
	data := ctx.DataType
	singular := strings.ToLower(typ)

	b.WriteString(goBanner())
	b.WriteString("package ")
	b.WriteString(pkg)
	b.WriteString("\n\n")
	std := []string{"context", "strconv"}
	if fieldsUse(ctx.Fields, FieldTime) {
		std = append(std, "time")
	}
	third := []string{
		"github.com/gombit-dev/gombit/contract",
		"github.com/gombit-dev/gombit/database",
	}
	if fieldsUse(ctx.Fields, FieldDecimal) {
		third = append(third, gombitTypesImport)
	}
	third = append(third, "gorm.io/gorm")
	b.WriteString(importBlock(std, third))
	b.WriteString("// Handler serves " + pkg + " HTTP operations over GORM.\n")
	b.WriteString("type Handler struct {\n\tDB *gorm.DB\n}\n\n")
	b.WriteString("type " + data + " struct {\n")
	b.WriteString("\tID uint `json:\"id\" example:\"1\" doc:\"" + typ + " identifier\"`\n")
	for _, field := range ctx.Fields {
		if !field.inDTO() {
			continue
		}
		b.WriteString("\t" + field.dtoGoName() + " " + field.dtoGoType() + " `json:\"" + field.dtoJSONName() + "\" doc:\"" + field.dtoGoName() + "\"`\n")
	}
	b.WriteString("}\n\n")
	filters := filterFields(ctx.Fields)
	searchCols := searchColumns(ctx.Fields)
	sortCols := sortColumns(ctx.Fields)
	aggFields := aggregateFields(ctx.Fields)
	// A resource with aggregatable fields returns aggregates in meta, so its list
	// meta is contract.ListMeta (PageMeta + optional aggregates); otherwise the
	// plain contract.PageMeta is unchanged, keeping a no-modifier resource's
	// output byte-identical.
	metaType := "contract.PageMeta"
	if len(aggFields) > 0 {
		metaType = "contract.ListMeta"
	}
	listOut := "list" + ctx.Resource.Tag + "Output"
	listIn := "list" + ctx.Resource.Tag + "Input"
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
		b.WriteString("\tAggregate string `query:\"aggregate\" doc:\"Comma-separated <func>:<field> aggregates over the filtered set, e.g. sum:" + aggFields[0].aggregateColumn() + " (funcs: sum, avg, min, max; fields: " + strings.Join(aggregateColumns(aggFields), ", ") + ")\"`\n")
	}
	for _, f := range filters {
		b.WriteString("\t" + f.filterInputField() + " string `" + f.filterQueryTag() + "`\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("type get" + typ + "Input struct {\n")
	b.WriteString("\tID string `path:\"id\" doc:\"" + typ + " identifier\"`\n}\n\n")
	b.WriteString("type get" + typ + "Output struct {\n")
	b.WriteString("\tBody contract.Data[" + data + "]\n}\n\n")
	b.WriteString("type create" + typ + "Input struct {\n\tBody struct {\n")
	for _, field := range ctx.Fields {
		if !field.inDTO() {
			continue
		}
		b.WriteString("\t\t" + field.dtoGoName() + " " + field.dtoGoType() + " `" + field.dtoHumaTags() + "`\n")
	}
	b.WriteString("\t}\n}\n\n")
	b.WriteString("type create" + typ + "Output struct {\n")
	b.WriteString("\tBody contract.Data[" + data + "]\n}\n\n")

	b.WriteString("func (h *Handler) list(ctx context.Context, input *" + listIn + ") (*" + listOut + ", error) {\n")
	b.WriteString("\tpage, perPage := contract.ClampPage(input.Page, input.PerPage)\n")
	b.WriteString("\tq := h.DB.WithContext(ctx).Model(&" + typ + "{})\n")
	// Declared filters and search narrow the set before the count, so meta.total
	// reflects the filtered collection, not the whole table. errDeclared tracks
	// whether the function-scope err exists yet so the first assignment uses :=.
	errDeclared := false
	for _, f := range filters {
		assign := "="
		if !errDeclared {
			assign = ":="
			errDeclared = true
		}
		b.WriteString("\tq, err " + assign + " database.FilterEq(ctx, q, \"" + f.filterColumn() + "\", " + f.filterKindExpr() + ", input." + f.filterInputField() + ")\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
	}
	if len(searchCols) > 0 {
		b.WriteString("\tq = database.Search(q, []string{\"" + strings.Join(searchCols, "\", \"") + "\"}, input.Search)\n")
	}
	b.WriteString("\tvar total int64\n")
	b.WriteString("\tif err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {\n")
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.Internal(\"list " + ctx.Resource.PluralSnake + "\"))\n")
	b.WriteString("\t}\n")
	if len(aggFields) > 0 {
		// Aggregates run over the same filtered/searched query as the count,
		// before pagination — so meta.aggregates covers the whole matching set,
		// not one page (issue #272). ParseAggregates declares err (aggs is new,
		// so := is always valid); mark it declared so a later Ordering reuses it.
		b.WriteString("\taggs, err := database.ParseAggregates(ctx, input.Aggregate, map[string]database.AggregateColumn{\n")
		for _, f := range aggFields {
			b.WriteString("\t\t\"" + f.aggregateColumn() + "\": {Column: \"" + f.aggregateColumn() + "\"},\n")
		}
		b.WriteString("\t})\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		b.WriteString("\taggregates, err := database.Aggregate(ctx, q.Session(&gorm.Session{}), aggs)\n")
		b.WriteString("\tif err != nil {\n\t\treturn nil, err\n\t}\n")
		errDeclared = true
	}
	if len(sortCols) > 0 {
		// Declared ordering replaces the fixed Order("id"), which stays the
		// fallback when ?ordering= is absent so the default page order is
		// unchanged. Same `?ordering=<field>` / `-<field>` spelling as the admin
		// data plane.
		// Ordering is the last consumer of the function-scope err, so this branch
		// only reads errDeclared (never needs to set it).
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
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.Internal(\"list " + ctx.Resource.PluralSnake + "\"))\n")
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
	b.WriteString("\t\t},\n")
	b.WriteString("\t}, nil\n}\n\n")

	b.WriteString("func (h *Handler) get(ctx context.Context, input *get" + typ + "Input) (*get" + typ + "Output, error) {\n")
	b.WriteString("\tid, err := strconv.ParseUint(input.ID, 10, 64)\n")
	b.WriteString("\tif err != nil {\n")
	b.WriteString("\t\treturn nil, contract.WithContext(ctx, contract.NotFound(\"" + singular + " not found\"))\n")
	b.WriteString("\t}\n")
	b.WriteString("\tvar row " + typ + "\n")
	b.WriteString("\tif err := h.DB.WithContext(ctx).First(&row, uint(id)).Error; err != nil {\n")
	b.WriteString("\t\treturn nil, database.MapLoadError(ctx, err, \"" + singular + " not found\", \"load " + singular + "\")\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn &get" + typ + "Output{\n")
	b.WriteString("\t\tBody: contract.Data[" + data + "]{Data: to" + typ + "Data(row)},\n")
	b.WriteString("\t}, nil\n}\n\n")

	b.WriteString("func (h *Handler) create(ctx context.Context, input *create" + typ + "Input) (*create" + typ + "Output, error) {\n")
	b.WriteString("\trow := " + typ + "{\n")
	for _, field := range ctx.Fields {
		if !field.inDTO() {
			continue
		}
		b.WriteString("\t\t" + field.dtoGoName() + ": input.Body." + field.dtoGoName() + ",\n")
	}
	b.WriteString("\t}\n")
	b.WriteString("\tif err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {\n")
	b.WriteString("\t\treturn nil, database.MapPersistError(ctx, err, \"resource already exists\", \"create " + singular + "\")\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn &create" + typ + "Output{\n")
	b.WriteString("\t\tBody: contract.Data[" + data + "]{Data: to" + typ + "Data(row)},\n")
	b.WriteString("\t}, nil\n}\n\n")

	b.WriteString("func to" + typ + "Data(row " + typ + ") " + data + " {\n")
	b.WriteString("\treturn " + data + "{ID: row.ID")
	for _, field := range ctx.Fields {
		if !field.inDTO() {
			continue
		}
		b.WriteString(", " + field.dtoGoName() + ": row." + field.dtoGoName())
	}
	b.WriteString("}\n}\n")
	return b.String()
}

func renderRoutes(ctx renderContext) string {
	var b strings.Builder
	pkg := ctx.Resource.Package
	typ := ctx.Resource.TypeName
	path := ctx.Resource.HTTPPath
	tag := ctx.Resource.Tag
	kebab := ctx.Resource.Kebab

	b.WriteString(goBanner())
	b.WriteString("package " + pkg + "\n\n")
	b.WriteString("import (\n\t\"net/http\"\n\n")
	b.WriteString("\t\"github.com/danielgtaylor/huma/v2\"\n")
	b.WriteString("\t\"github.com/gombit-dev/gombit/framework\"\n)\n\n")
	b.WriteString("// Register mounts " + pkg + " Huma routes. Called explicitly from main; Gombit\n")
	b.WriteString("// does not discover feature packages by reflection.\n")
	b.WriteString("func Register(app *framework.App) {\n")
	b.WriteString("\th := &Handler{DB: app.DB()}\n")
	b.WriteString("\tprefix := app.Config().API.Prefix\n")
	b.WriteString("\tapi := app.API()\n\n")
	b.WriteString("\thuma.Register(api, huma.Operation{\n")
	b.WriteString("\t\tOperationID: \"list-" + kebab + "\",\n")
	b.WriteString("\t\tMethod:      http.MethodGet,\n")
	b.WriteString("\t\tPath:        prefix + \"" + path + "\",\n")
	b.WriteString("\t\tSummary:     \"List " + strings.ToLower(tag) + "\",\n")
	b.WriteString("\t\tTags:        []string{\"" + tag + "\"},\n")
	b.WriteString("\t}, h.list)\n\n")
	b.WriteString("\thuma.Register(api, huma.Operation{\n")
	b.WriteString("\t\tOperationID: \"get-" + pkg + "\",\n")
	b.WriteString("\t\tMethod:      http.MethodGet,\n")
	b.WriteString("\t\tPath:        prefix + \"" + path + "/{id}\",\n")
	b.WriteString("\t\tSummary:     \"Get a " + strings.ToLower(typ) + "\",\n")
	b.WriteString("\t\tTags:        []string{\"" + tag + "\"},\n")
	b.WriteString("\t}, h.get)\n\n")
	b.WriteString("\thuma.Register(api, huma.Operation{\n")
	b.WriteString("\t\tOperationID: \"create-" + pkg + "\",\n")
	b.WriteString("\t\tMethod:      http.MethodPost,\n")
	b.WriteString("\t\tPath:        prefix + \"" + path + "\",\n")
	b.WriteString("\t\tSummary:     \"Create a " + strings.ToLower(typ) + "\",\n")
	b.WriteString("\t\tTags:        []string{\"" + tag + "\"},\n")
	b.WriteString("\t}, h.create)\n")
	b.WriteString("}\n")
	return b.String()
}

func renderService(ctx renderContext) string {
	typ := ctx.Resource.TypeName
	pkg := ctx.Resource.Package
	var b strings.Builder
	b.WriteString(goBanner())
	b.WriteString("package " + pkg + "\n\n")
	b.WriteString("import (\n\t\"context\"\n\n\t\"gorm.io/gorm\"\n)\n\n")
	b.WriteString("// Service is an opt-in pass-through over GORM (--service). The generated\n")
	b.WriteString("// handler stays thin over GORM; this type exists so the file compiles.\n")
	b.WriteString("type Service struct {\n\tDB *gorm.DB\n}\n\n")
	b.WriteString("func NewService(db *gorm.DB) *Service {\n\treturn &Service{DB: db}\n}\n\n")
	b.WriteString("func (s *Service) List(ctx context.Context) ([]" + typ + ", error) {\n")
	b.WriteString("\tvar rows []" + typ + "\n")
	b.WriteString("\terr := s.DB.WithContext(ctx).Order(\"id\").Find(&rows).Error\n")
	b.WriteString("\treturn rows, err\n}\n\n")
	b.WriteString("func (s *Service) Get(ctx context.Context, id uint) (" + typ + ", error) {\n")
	b.WriteString("\tvar row " + typ + "\n")
	b.WriteString("\terr := s.DB.WithContext(ctx).First(&row, id).Error\n")
	b.WriteString("\treturn row, err\n}\n\n")
	b.WriteString("func (s *Service) Create(ctx context.Context, row *" + typ + ") error {\n")
	b.WriteString("\treturn s.DB.WithContext(ctx).Create(row).Error\n}\n")
	return b.String()
}

func renderRepo(ctx renderContext) string {
	typ := ctx.Resource.TypeName
	pkg := ctx.Resource.Package
	var b strings.Builder
	b.WriteString(goBanner())
	b.WriteString("package " + pkg + "\n\n")
	b.WriteString("import (\n\t\"context\"\n\n\t\"gorm.io/gorm\"\n)\n\n")
	b.WriteString("// Repo is an opt-in pass-through over GORM (--repo). Prefer the runtime\n")
	b.WriteString("// repository.New[T] helper instead of growing this file (D9).\n")
	b.WriteString("type Repo struct {\n\tDB *gorm.DB\n}\n\n")
	b.WriteString("func NewRepo(db *gorm.DB) *Repo {\n\treturn &Repo{DB: db}\n}\n\n")
	b.WriteString("func (r *Repo) List(ctx context.Context) ([]" + typ + ", error) {\n")
	b.WriteString("\tvar rows []" + typ + "\n")
	b.WriteString("\terr := r.DB.WithContext(ctx).Order(\"id\").Find(&rows).Error\n")
	b.WriteString("\treturn rows, err\n}\n\n")
	b.WriteString("func (r *Repo) Get(ctx context.Context, id uint) (" + typ + ", error) {\n")
	b.WriteString("\tvar row " + typ + "\n")
	b.WriteString("\terr := r.DB.WithContext(ctx).First(&row, id).Error\n")
	b.WriteString("\treturn row, err\n}\n\n")
	b.WriteString("func (r *Repo) Create(ctx context.Context, row *" + typ + ") error {\n")
	b.WriteString("\treturn r.DB.WithContext(ctx).Create(row).Error\n}\n")
	return b.String()
}

func jsIdent(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

func renderListTSX(ctx renderContext) string {
	if ctx.UI == "mui" {
		return renderMUIListTSX(ctx)
	}
	return renderMinimalListTSX(ctx)
}

func renderMinimalListTSX(ctx renderContext) string {
	listPath := defaultAPIPrefix + ctx.Resource.HTTPPath
	labels := `["id"`
	for _, field := range ctx.Fields {
		labels += `, "` + field.JSONName + `"`
	}
	labels += "]"

	var b strings.Builder
	b.WriteString(tsBanner())
	b.WriteString("\nimport { useEffect, useState } from \"react\";\n")
	b.WriteString("import { Link } from \"react-router\";\n\n")
	b.WriteString("import { useApiClient } from \"../api/client\";\n")
	b.WriteString("import { unwrap } from \"../api/generated/client\";\n")
	b.WriteString("import type { paths } from \"../api/generated/schema\";\n\n")
	b.WriteString("const listPath = \"" + listPath + "\" as const;\n\n")
	b.WriteString("type ListResponse =\n")
	b.WriteString("  paths[typeof listPath][\"get\"][\"responses\"][200][\"content\"][\"application/json\"];\n")
	b.WriteString("type ListRow = NonNullable<ListResponse[\"data\"]>[number];\n\n")
	b.WriteString("/**\n * React list/table page. Types come from the generated OpenAPI client\n")
	b.WriteString(" * (gombit client generate / gombit dev). Do not duplicate API DTOs here.\n */\n")
	b.WriteString("export function " + ctx.Resource.TypeName + "ListPage() {\n")
	b.WriteString("  const client = useApiClient();\n")
	b.WriteString("  const [rows, setRows] = useState<ListRow[]>([]);\n")
	b.WriteString("  const [status, setStatus] = useState(\"Loading…\");\n\n")
	b.WriteString("  useEffect(() => {\n")
	b.WriteString("    let cancelled = false;\n")
	b.WriteString("    void (async () => {\n")
	b.WriteString("      try {\n")
	b.WriteString("        const listed = await unwrap(await client.GET(listPath));\n")
	b.WriteString("        if (cancelled) {\n")
	b.WriteString("          return;\n")
	b.WriteString("        }\n")
	b.WriteString("        const data = Array.isArray(listed.data) ? listed.data : [];\n")
	b.WriteString("        setRows(data);\n")
	b.WriteString("        setStatus(data.length === 0 ? \"No " + ctx.Resource.Kebab + " yet.\" : \"\");\n")
	b.WriteString("      } catch (err: unknown) {\n")
	b.WriteString("        if (cancelled) {\n")
	b.WriteString("          return;\n")
	b.WriteString("        }\n")
	b.WriteString("        setStatus(err instanceof Error ? err.message : \"request failed\");\n")
	b.WriteString("      }\n")
	b.WriteString("    })();\n")
	b.WriteString("    return () => {\n")
	b.WriteString("      cancelled = true;\n")
	b.WriteString("    };\n")
	b.WriteString("  }, [client]);\n\n")
	b.WriteString("  return (\n")
	b.WriteString("    <section>\n")
	b.WriteString("      <h1>" + ctx.Resource.Tag + "</h1>\n")
	b.WriteString("      <p>\n")
	b.WriteString("        <Link to=\"/" + ctx.Resource.Kebab + "/new\">New " + ctx.Resource.TypeName + "</Link>\n")
	b.WriteString("      </p>\n")
	b.WriteString("      <table>\n")
	b.WriteString("        <thead>\n")
	b.WriteString("          <tr>\n")
	b.WriteString("            {" + labels + ".map((label) => (\n")
	b.WriteString("              <th key={label}>{label}</th>\n")
	b.WriteString("            ))}\n")
	b.WriteString("          </tr>\n")
	b.WriteString("        </thead>\n")
	b.WriteString("        <tbody>\n")
	b.WriteString("          {rows.map((row, index) => {\n")
	b.WriteString("            const record = row as { id?: unknown")
	for _, field := range ctx.Fields {
		b.WriteString("; " + field.JSONName + "?: unknown")
	}
	b.WriteString(" };\n")
	b.WriteString("            const values: unknown[] = [record.id")
	for _, field := range ctx.Fields {
		b.WriteString(", record." + field.JSONName)
	}
	b.WriteString("];\n")
	b.WriteString("            const key = record.id == null ? String(index) : String(record.id);\n")
	b.WriteString("            return (\n")
	b.WriteString("              <tr key={key}>\n")
	b.WriteString("                {values.map((value, cell) => (\n")
	b.WriteString("                  <td key={cell}>{value == null ? \"\" : String(value)}</td>\n")
	b.WriteString("                ))}\n")
	b.WriteString("              </tr>\n")
	b.WriteString("            );\n")
	b.WriteString("          })}\n")
	b.WriteString("        </tbody>\n")
	b.WriteString("      </table>\n")
	b.WriteString("      {status ? <p>{status}</p> : null}\n")
	b.WriteString("    </section>\n")
	b.WriteString("  );\n")
	b.WriteString("}\n")
	return b.String()
}

func renderFormTSX(ctx renderContext) string {
	if ctx.UI == "mui" {
		return renderMUIFormTSX(ctx)
	}
	return renderMinimalFormTSX(ctx)
}

func renderMinimalFormTSX(ctx renderContext) string {
	createPath := defaultAPIPrefix + ctx.Resource.HTTPPath
	var b strings.Builder
	b.WriteString(tsBanner())
	b.WriteString("\nimport { useState } from \"react\";\n")
	b.WriteString("import { useForm } from \"react-hook-form\";\n")
	b.WriteString("import { Link, useNavigate } from \"react-router\";\n\n")
	b.WriteString("import { useApiClient } from \"../api/client\";\n")
	b.WriteString("import { applyContractErrors } from \"../api/formErrors\";\n")
	b.WriteString("import { unwrap } from \"../api/generated/client\";\n")
	b.WriteString("import type { paths } from \"../api/generated/schema\";\n\n")
	b.WriteString("const createPath = \"" + createPath + "\" as const;\n\n")
	b.WriteString("type CreateBody =\n")
	b.WriteString("  paths[typeof createPath][\"post\"][\"requestBody\"][\"content\"][\"application/json\"];\n\n")
	b.WriteString("type FormValues = {\n")
	for _, field := range ctx.Fields {
		b.WriteString("  " + field.JSONName + ": " + tsFormType(field) + ";\n")
	}
	b.WriteString("};\n\n")
	b.WriteString("/**\n * React Hook Form create page. Request/response types come from the\n")
	b.WriteString(" * generated OpenAPI client. D10 error.fields map through applyContractErrors.\n")
	b.WriteString(" * Run gombit client generate or gombit dev after adding routes.\n */\n")
	b.WriteString("export function " + ctx.Resource.TypeName + "FormPage() {\n")
	b.WriteString("  const client = useApiClient();\n")
	b.WriteString("  const navigate = useNavigate();\n")
	b.WriteString("  const [status, setStatus] = useState(\"\");\n")
	b.WriteString("  const {\n")
	b.WriteString("    register,\n")
	b.WriteString("    handleSubmit,\n")
	b.WriteString("    setError,\n")
	b.WriteString("    formState: { errors, isSubmitting },\n")
	b.WriteString("  } = useForm<FormValues>({\n")
	b.WriteString("    defaultValues: {")
	for i, field := range ctx.Fields {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + field.JSONName + ": " + tsDefaultValue(field))
	}
	b.WriteString(" },\n")
	b.WriteString("  });\n\n")
	b.WriteString("  async function onSubmit(values: FormValues) {\n")
	b.WriteString("    setStatus(\"\");\n")
	b.WriteString("    try {\n")
	b.WriteString("      await unwrap(await client.POST(createPath, { body: values as CreateBody }));\n")
	b.WriteString("      navigate(\"/" + ctx.Resource.Kebab + "\");\n")
	b.WriteString("    } catch (err: unknown) {\n")
	b.WriteString("      if (!applyContractErrors(setError, err)) {\n")
	b.WriteString("        setStatus(err instanceof Error ? err.message : \"request failed\");\n")
	b.WriteString("      }\n")
	b.WriteString("    }\n")
	b.WriteString("  }\n\n")
	b.WriteString("  return (\n")
	b.WriteString("    <section>\n")
	b.WriteString("      <h1>New " + ctx.Resource.TypeName + "</h1>\n")
	b.WriteString("      <p>\n")
	b.WriteString("        <Link to=\"/" + ctx.Resource.Kebab + "\">Back to list</Link>\n")
	b.WriteString("      </p>\n")
	b.WriteString("      <form onSubmit={handleSubmit(onSubmit)}>\n")
	for _, field := range ctx.Fields {
		b.WriteString(renderFormField(field))
	}
	b.WriteString("        <button type=\"submit\" disabled={isSubmitting}>\n")
	b.WriteString("          Create\n")
	b.WriteString("        </button>\n")
	b.WriteString("      </form>\n")
	b.WriteString("      {status ? <p>{status}</p> : null}\n")
	b.WriteString("    </section>\n")
	b.WriteString("  );\n")
	b.WriteString("}\n")
	return b.String()
}

// tsStringArray renders a TS array literal of double-quoted strings.
func tsStringArray(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, `"`+n+`"`)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func tsFormType(field Field) string {
	switch field.Type {
	case FieldBool:
		return "boolean"
	case FieldInt, FieldInt64, FieldUint:
		return "number"
	case FieldEnum:
		return tsEnumUnion(field)
	default:
		// decimal + time are carried as strings (JSON string / RFC3339).
		return "string"
	}
}

// tsEnumUnion renders a TypeScript string-literal union for an enum field.
func tsEnumUnion(field Field) string {
	quoted := make([]string, 0, len(field.EnumValues))
	for _, v := range field.EnumValues {
		quoted = append(quoted, `"`+v+`"`)
	}
	return strings.Join(quoted, " | ")
}

func tsDefaultValue(field Field) string {
	switch field.Type {
	case FieldBool:
		return "false"
	case FieldInt, FieldInt64, FieldUint:
		return "0"
	case FieldEnum:
		if len(field.EnumValues) > 0 {
			return `"` + field.EnumValues[0] + `"`
		}
		return `""`
	default:
		return `""`
	}
}

func renderFormField(field Field) string {
	ident := jsIdent(field.JSONName)
	var b strings.Builder
	b.WriteString("        <label>\n")
	b.WriteString("          " + field.GoName + "\n")
	switch field.Type {
	case FieldText:
		b.WriteString("          <textarea {...register(\"" + field.JSONName + "\"")
		if field.Required {
			b.WriteString(", { required: \"" + field.GoName + " is required\" }")
		}
		b.WriteString(")} />\n")
	case FieldBool:
		b.WriteString("          <input type=\"checkbox\" {...register(\"" + field.JSONName + "\")} />\n")
	case FieldInt, FieldInt64, FieldUint:
		b.WriteString("          <input type=\"number\" {...register(\"" + field.JSONName + "\", { setValueAs: (value) => (value === \"\" ? 0 : Number(value)) })} />\n")
	case FieldEnum:
		b.WriteString("          <select {...register(\"" + field.JSONName + "\")}>\n")
		for _, v := range field.EnumValues {
			b.WriteString("            <option value=\"" + v + "\">" + v + "</option>\n")
		}
		b.WriteString("          </select>\n")
	case FieldTime:
		// datetime-local yields "YYYY-MM-DDTHH:mm" (local wall time); the input
		// displays the raw typed value, and setValueAs converts to RFC3339 UTC on
		// submit — empty becomes null so an optional (*time.Time) field can be
		// left blank.
		b.WriteString("          <input type=\"datetime-local\" {...register(\"" + field.JSONName + "\", { setValueAs: (value) => (value === \"\" ? null : new Date(value).toISOString())")
		if field.Required {
			b.WriteString(", required: \"" + field.GoName + " is required\"")
		}
		b.WriteString(" })} />\n")
	case FieldDecimal:
		// Empty becomes null so an optional (*types.Decimal) field round-trips; a
		// non-empty value is sent as the exact decimal string.
		b.WriteString("          <input type=\"text\" inputMode=\"decimal\" {...register(\"" + field.JSONName + "\", { setValueAs: (value) => (value === \"\" ? null : value)")
		if field.Required {
			b.WriteString(", required: \"" + field.GoName + " is required\"")
		}
		b.WriteString(" })} />\n")
	default:
		b.WriteString("          <input type=\"text\" {...register(\"" + field.JSONName + "\"")
		if field.Required {
			b.WriteString(", { required: \"" + field.GoName + " is required\" }")
		}
		b.WriteString(")} />\n")
	}
	b.WriteString("        </label>\n")
	b.WriteString("        {errors." + ident + "?.message ? <p>{errors." + ident + ".message}</p> : null}\n")
	return b.String()
}

func renderMUIListTSX(ctx renderContext) string {
	listPath := defaultAPIPrefix + ctx.Resource.HTTPPath
	colSpan := 1 + len(ctx.Fields)
	labels := `["id"`
	for _, field := range ctx.Fields {
		labels += `, "` + field.JSONName + `"`
	}
	labels += "]"

	var b strings.Builder
	b.WriteString(tsBanner())
	b.WriteString("\nimport { useEffect, useState } from \"react\";\n")
	b.WriteString("import { Link } from \"react-router\";\n")
	b.WriteString("import AddIcon from \"@mui/icons-material/Add\";\n")
	b.WriteString("import {\n")
	b.WriteString("  Box,\n  Button,\n  CircularProgress,\n  Paper,\n")
	b.WriteString("  Table,\n  TableBody,\n  TableCell,\n  TableContainer,\n")
	b.WriteString("  TableHead,\n  TableRow,\n  Typography,\n")
	b.WriteString("} from \"@mui/material\";\n\n")
	b.WriteString("import { useApiClient } from \"../api/client\";\n")
	b.WriteString("import { unwrap } from \"../api/generated/client\";\n")
	b.WriteString("import type { paths } from \"../api/generated/schema\";\n\n")
	b.WriteString("const listPath = \"" + listPath + "\" as const;\n\n")
	b.WriteString("type ListResponse =\n")
	b.WriteString("  paths[typeof listPath][\"get\"][\"responses\"][200][\"content\"][\"application/json\"];\n")
	b.WriteString("type ListRow = NonNullable<ListResponse[\"data\"]>[number];\n\n")
	b.WriteString("/**\n * MUI Table list page. Types come from the generated OpenAPI client\n")
	b.WriteString(" * (gombit client generate / gombit dev). Do not duplicate API DTOs here.\n */\n")
	b.WriteString("export function " + ctx.Resource.TypeName + "ListPage() {\n")
	b.WriteString("  const client = useApiClient();\n")
	b.WriteString("  const [rows, setRows] = useState<ListRow[]>([]);\n")
	b.WriteString("  const [loading, setLoading] = useState(true);\n")
	b.WriteString("  const [status, setStatus] = useState(\"\");\n\n")
	b.WriteString("  useEffect(() => {\n")
	b.WriteString("    let cancelled = false;\n")
	b.WriteString("    void (async () => {\n")
	b.WriteString("      try {\n")
	b.WriteString("        const listed = await unwrap(await client.GET(listPath));\n")
	b.WriteString("        if (cancelled) {\n")
	b.WriteString("          return;\n")
	b.WriteString("        }\n")
	b.WriteString("        const data = Array.isArray(listed.data) ? listed.data : [];\n")
	b.WriteString("        setRows(data);\n")
	b.WriteString("        setStatus(data.length === 0 ? \"No " + ctx.Resource.Kebab + " yet.\" : \"\");\n")
	b.WriteString("      } catch (err: unknown) {\n")
	b.WriteString("        if (cancelled) {\n")
	b.WriteString("          return;\n")
	b.WriteString("        }\n")
	b.WriteString("        setStatus(err instanceof Error ? err.message : \"request failed\");\n")
	b.WriteString("      } finally {\n")
	b.WriteString("        if (!cancelled) {\n")
	b.WriteString("          setLoading(false);\n")
	b.WriteString("        }\n")
	b.WriteString("      }\n")
	b.WriteString("    })();\n")
	b.WriteString("    return () => {\n")
	b.WriteString("      cancelled = true;\n")
	b.WriteString("    };\n")
	b.WriteString("  }, [client]);\n\n")
	b.WriteString("  return (\n")
	b.WriteString("    <Box>\n")
	b.WriteString("      <Box sx={{ display: \"flex\", justifyContent: \"space-between\", alignItems: \"center\", mb: 2 }}>\n")
	b.WriteString("        <Typography variant=\"h4\" component=\"h1\">\n")
	b.WriteString("          " + ctx.Resource.Tag + "\n")
	b.WriteString("        </Typography>\n")
	b.WriteString("        <Button variant=\"contained\" component={Link} to=\"/" + ctx.Resource.Kebab + "/new\" startIcon={<AddIcon />}>\n")
	b.WriteString("          New " + ctx.Resource.TypeName + "\n")
	b.WriteString("        </Button>\n")
	b.WriteString("      </Box>\n")
	b.WriteString("      {loading ? (\n")
	b.WriteString("        <Box sx={{ display: \"flex\", justifyContent: \"center\", py: 6 }}>\n")
	b.WriteString("          <CircularProgress />\n")
	b.WriteString("        </Box>\n")
	b.WriteString("      ) : (\n")
	b.WriteString("        <TableContainer component={Paper}>\n")
	b.WriteString("          <Table>\n")
	b.WriteString("            <TableHead>\n")
	b.WriteString("              <TableRow>\n")
	b.WriteString("                {" + labels + ".map((label) => (\n")
	b.WriteString("                  <TableCell key={label}>{label}</TableCell>\n")
	b.WriteString("                ))}\n")
	b.WriteString("              </TableRow>\n")
	b.WriteString("            </TableHead>\n")
	b.WriteString("            <TableBody>\n")
	b.WriteString("              {rows.length === 0 ? (\n")
	b.WriteString("                <TableRow>\n")
	b.WriteString("                  <TableCell colSpan={" + fmt.Sprintf("%d", colSpan) + "} align=\"center\">\n")
	b.WriteString("                    {status || \"No " + ctx.Resource.Kebab + " yet.\"}\n")
	b.WriteString("                  </TableCell>\n")
	b.WriteString("                </TableRow>\n")
	b.WriteString("              ) : (\n")
	b.WriteString("                rows.map((row, index) => {\n")
	b.WriteString("                  const record = row as { id?: unknown")
	for _, field := range ctx.Fields {
		b.WriteString("; " + field.JSONName + "?: unknown")
	}
	b.WriteString(" };\n")
	b.WriteString("                  const values: unknown[] = [record.id")
	for _, field := range ctx.Fields {
		b.WriteString(", record." + field.JSONName)
	}
	b.WriteString("];\n")
	b.WriteString("                  const key = record.id == null ? String(index) : String(record.id);\n")
	b.WriteString("                  return (\n")
	b.WriteString("                    <TableRow key={key}>\n")
	b.WriteString("                      {values.map((value, cell) => (\n")
	b.WriteString("                        <TableCell key={cell}>{value == null ? \"\" : String(value)}</TableCell>\n")
	b.WriteString("                      ))}\n")
	b.WriteString("                    </TableRow>\n")
	b.WriteString("                  );\n")
	b.WriteString("                })\n")
	b.WriteString("              )}\n")
	b.WriteString("            </TableBody>\n")
	b.WriteString("          </Table>\n")
	b.WriteString("        </TableContainer>\n")
	b.WriteString("      )}\n")
	b.WriteString("    </Box>\n")
	b.WriteString("  );\n")
	b.WriteString("}\n")
	return b.String()
}

func renderMUIFormTSX(ctx renderContext) string {
	createPath := defaultAPIPrefix + ctx.Resource.HTTPPath
	needsCheckbox := fieldsUse(ctx.Fields, FieldBool)
	needsSelect := fieldsUse(ctx.Fields, FieldEnum)

	var b strings.Builder
	b.WriteString(tsBanner())
	b.WriteString("\nimport { useState } from \"react\";\n")
	b.WriteString("import { Controller, useForm } from \"react-hook-form\";\n")
	b.WriteString("import { Link, useNavigate } from \"react-router\";\n")
	b.WriteString("import { Alert, Box, Button, Paper, TextField, Typography")
	if needsCheckbox {
		b.WriteString(", Checkbox, FormControlLabel")
	}
	if needsSelect {
		b.WriteString(", MenuItem")
	}
	b.WriteString(" } from \"@mui/material\";\n\n")
	b.WriteString("import { useApiClient } from \"../api/client\";\n")
	b.WriteString("import { applyContractErrors } from \"../api/formErrors\";\n")
	b.WriteString("import { unwrap } from \"../api/generated/client\";\n")
	b.WriteString("import type { paths } from \"../api/generated/schema\";\n\n")
	b.WriteString("const createPath = \"" + createPath + "\" as const;\n\n")
	b.WriteString("type CreateBody =\n")
	b.WriteString("  paths[typeof createPath][\"post\"][\"requestBody\"][\"content\"][\"application/json\"];\n\n")
	b.WriteString("type FormValues = {\n")
	for _, field := range ctx.Fields {
		b.WriteString("  " + field.JSONName + ": " + tsFormType(field) + ";\n")
	}
	b.WriteString("};\n\n")
	b.WriteString("/**\n * MUI TextField create page. Request/response types come from the\n")
	b.WriteString(" * generated OpenAPI client. D10 error.fields map through applyContractErrors.\n")
	b.WriteString(" * Run gombit client generate or gombit dev after adding routes.\n */\n")
	b.WriteString("export function " + ctx.Resource.TypeName + "FormPage() {\n")
	b.WriteString("  const client = useApiClient();\n")
	b.WriteString("  const navigate = useNavigate();\n")
	b.WriteString("  const [status, setStatus] = useState(\"\");\n")
	b.WriteString("  const {\n")
	b.WriteString("    control,\n")
	b.WriteString("    handleSubmit,\n")
	b.WriteString("    setError,\n")
	b.WriteString("    formState: { isSubmitting },\n")
	b.WriteString("  } = useForm<FormValues>({\n")
	b.WriteString("    defaultValues: {")
	for i, field := range ctx.Fields {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" " + field.JSONName + ": " + tsDefaultValue(field))
	}
	b.WriteString(" },\n")
	b.WriteString("  });\n\n")
	var timeNames, decimalNames []string
	for _, field := range ctx.Fields {
		switch field.Type {
		case FieldTime:
			timeNames = append(timeNames, field.JSONName)
		case FieldDecimal:
			decimalNames = append(decimalNames, field.JSONName)
		}
	}
	b.WriteString("  async function onSubmit(values: FormValues) {\n")
	b.WriteString("    setStatus(\"\");\n")
	bodyExpr := "values as CreateBody"
	if len(timeNames) > 0 || len(decimalNames) > 0 {
		b.WriteString("    const body: Record<string, unknown> = { ...values };\n")
		if len(timeNames) > 0 {
			// Local datetime-local -> RFC3339 UTC; empty -> null (optional field).
			b.WriteString("    " + tsStringArray(timeNames) + ".forEach((key) => {\n")
			b.WriteString("      const v = body[key];\n")
			b.WriteString("      body[key] = v == null || v === \"\" ? null : new Date(String(v)).toISOString();\n")
			b.WriteString("    });\n")
		}
		if len(decimalNames) > 0 {
			// Empty decimal string -> null (optional *types.Decimal).
			b.WriteString("    " + tsStringArray(decimalNames) + ".forEach((key) => {\n")
			b.WriteString("      if (body[key] == null || body[key] === \"\") body[key] = null;\n")
			b.WriteString("    });\n")
		}
		bodyExpr = "body as CreateBody"
	}
	b.WriteString("    try {\n")
	b.WriteString("      await unwrap(await client.POST(createPath, { body: " + bodyExpr + " }));\n")
	b.WriteString("      navigate(\"/" + ctx.Resource.Kebab + "\");\n")
	b.WriteString("    } catch (err: unknown) {\n")
	b.WriteString("      if (!applyContractErrors(setError, err)) {\n")
	b.WriteString("        setStatus(err instanceof Error ? err.message : \"request failed\");\n")
	b.WriteString("      }\n")
	b.WriteString("    }\n")
	b.WriteString("  }\n\n")
	b.WriteString("  return (\n")
	b.WriteString("    <Box>\n")
	b.WriteString("      <Typography variant=\"h4\" component=\"h1\" sx={{ mb: 1 }}>\n")
	b.WriteString("        New " + ctx.Resource.TypeName + "\n")
	b.WriteString("      </Typography>\n")
	b.WriteString("      <Button component={Link} to=\"/" + ctx.Resource.Kebab + "\" sx={{ mb: 2 }}>\n")
	b.WriteString("        Back to list\n")
	b.WriteString("      </Button>\n")
	b.WriteString("      <Paper sx={{ p: 3, maxWidth: 480 }}>\n")
	b.WriteString("        <Box component=\"form\" onSubmit={handleSubmit(onSubmit)} sx={{ display: \"flex\", flexDirection: \"column\", gap: 2 }}>\n")
	for _, field := range ctx.Fields {
		b.WriteString(renderMUIFormField(field))
	}
	b.WriteString("          <Button type=\"submit\" variant=\"contained\" disabled={isSubmitting}>\n")
	b.WriteString("            Create\n")
	b.WriteString("          </Button>\n")
	b.WriteString("        </Box>\n")
	b.WriteString("        {status ? (\n")
	b.WriteString("          <Alert severity=\"error\" sx={{ mt: 2 }}>\n")
	b.WriteString("            {status}\n")
	b.WriteString("          </Alert>\n")
	b.WriteString("        ) : null}\n")
	b.WriteString("      </Paper>\n")
	b.WriteString("    </Box>\n")
	b.WriteString("  );\n")
	b.WriteString("}\n")
	return b.String()
}

func renderMUIFormField(field Field) string {
	var b strings.Builder
	rules := ""
	if field.Required && field.Type != FieldBool {
		rules = " rules={{ required: \"" + field.GoName + " is required\" }}"
	}
	b.WriteString("          <Controller\n")
	b.WriteString("            name=\"" + field.JSONName + "\"\n")
	b.WriteString("            control={control}\n")
	if rules != "" {
		b.WriteString("           " + rules + "\n")
	}
	b.WriteString("            render={({ field, fieldState }) => (\n")
	switch field.Type {
	case FieldBool:
		b.WriteString("              <FormControlLabel\n")
		b.WriteString("                control={\n")
		b.WriteString("                  <Checkbox\n")
		b.WriteString("                    {...field}\n")
		b.WriteString("                    checked={Boolean(field.value)}\n")
		b.WriteString("                    disabled={isSubmitting}\n")
		b.WriteString("                  />\n")
		b.WriteString("                }\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("              />\n")
	case FieldText:
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                multiline\n")
		b.WriteString("                minRows={3}\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              />\n")
	case FieldInt, FieldInt64, FieldUint:
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                type=\"number\"\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("                onChange={(event) => {\n")
		b.WriteString("                  const raw = event.target.value;\n")
		b.WriteString("                  field.onChange(raw === \"\" ? 0 : Number(raw));\n")
		b.WriteString("                }}\n")
		b.WriteString("              />\n")
	case FieldEnum:
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                select\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              >\n")
		for _, v := range field.EnumValues {
			b.WriteString("                <MenuItem value=\"" + v + "\">" + v + "</MenuItem>\n")
		}
		b.WriteString("              </TextField>\n")
	case FieldTime:
		// Store the raw datetime-local (local wall time) so the picker shows what
		// the user chose; onSubmit converts it to RFC3339 UTC. Storing UTC ISO
		// here and slicing it back into a local input shifts the displayed time.
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                value={field.value ?? \"\"}\n")
		b.WriteString("                type=\"datetime-local\"\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                slotProps={{ inputLabel: { shrink: true } }}\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              />\n")
	case FieldDecimal:
		// The exact decimal string is submitted as-is; onSubmit nulls an empty
		// optional (*types.Decimal) value.
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                value={field.value ?? \"\"}\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                slotProps={{ htmlInput: { inputMode: \"decimal\" } }}\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              />\n")
	default:
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              />\n")
	}
	b.WriteString("            )}\n")
	b.WriteString("          />\n")
	return b.String()
}
