package resourcegen

import (
	"fmt"
	"go/format"
	"sort"
	"strconv"
	"strings"

	logical "github.com/gombit-dev/gombit/field"
)

type fileSpec struct {
	relPath string
	content []byte
	owned   bool // AST-edited user file; additive, not banner-gated
	// seedOnce marks a human-owned file scaffolded once and then owned by the
	// developer (the model): written when absent, preserved on regeneration, and
	// replaced only with --force. It carries no DO-NOT-EDIT banner.
	seedOnce bool
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
	// Model-first (ADR-016): make resource scaffolds the human-owned model and marks
	// the package as a resource; the generator-owned DTOs, mappers, and CRUD handler
	// (and the seed hooks file) are produced by gombit generate, which cli/make.go
	// runs right after this — so no human-owned handler.go / routes.go here. That
	// human-owned CRUD plumbing, and its generators, are what this redesign retires.
	files := []fileSpec{
		{relPath: fmt.Sprintf("internal/%s/%s.go", ctx.Resource.Package, ctx.Resource.FileBase), content: mustFormatGo(renderModel(ctx)), seedOnce: true},
		{relPath: fmt.Sprintf("internal/%s/%s", ctx.Resource.Package, ResourceMarkerFile), content: []byte(resourceMarkerContent(ctx.Resource.Package))},
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
	// The model is the human-owned source of truth (ADR-016): NO DO-NOT-EDIT banner
	// (make resource scaffolds it once and preserves it), unlike the generated
	// *.gen.go. The developer edits it; gombit generate re-derives the DTOs from it.
	b.WriteString("package ")
	b.WriteString(ctx.Resource.Package)
	b.WriteString("\n\n")

	var std, third []string
	if fieldsUse(ctx.Fields, FieldTime) {
		std = append(std, "time")
	}
	third = append(third, "gorm.io/gorm")
	if fieldsUse(ctx.Fields, FieldDecimal) || fieldsUse(ctx.Fields, FieldDate) || fieldsUse(ctx.Fields, FieldJSON) {
		third = append(third, gombitTypesImport)
	}
	if fieldsUse(ctx.Fields, FieldUUID) {
		third = append(third, "github.com/google/uuid")
	}
	third = append(third, targetImports(ctx)...)
	b.WriteString(importBlock(std, third))
	b.WriteString("// ")
	b.WriteString(ctx.Resource.TypeName)
	b.WriteString(" is the feature-package GORM model — the human-owned source of truth.\n")
	b.WriteString("// Edit its fields and their gombit:\"...\" policy, then run `gombit generate`\n")
	b.WriteString("// to re-derive the DTOs, mappers, and handler (*.gen.go).\n")
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
		if !f.isRelation() || f.Self {
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
	case FieldBelongsTo, FieldOneToOne:
		// The foreign key is the persisted column: read+write content and
		// filterable by default. one_to_one is that key with a unique index.
		// A nullable key is *uint so a blank is SQL NULL. The association
		// carries the delete rule; restrict is the default.
		fkType := "uint"
		fkGorm := "index"
		if f.Nullable {
			fkType = "*uint"
		}
		if f.Type == FieldOneToOne {
			fkGorm = "uniqueIndex"
		}
		onDelete := f.OnDelete
		if onDelete == "" {
			onDelete = "RESTRICT"
		}
		return "\t" + f.fkGoName() + " " + fkType + structTag(fkGorm, "read,write,filterable", "") + "\n" +
			"\t" + f.GoName + " " + f.GoType + structTag("constraint:OnUpdate:CASCADE,OnDelete:"+onDelete+";", "", "") + "\n"
	case FieldHasMany:
		return "\t" + f.GoName + " " + f.GoType + "\n"
	case FieldManyToMany:
		return "\t" + f.GoName + " " + f.GoType + structTag("many2many:"+f.joinTable(resourcePkg)+";", "", "") + "\n"
	default:
		return "\t" + f.GoName + " " + f.GoType + modelStructTag(f) + "\n"
	}
}

// resourceMarkerContent is the body of the .gombit-resource marker; the file's
// presence, not its content, marks the package a model-first resource.
func resourceMarkerContent(pkg string) string {
	return "# This file marks internal/" + pkg + " as a gombit model-first resource.\n" +
		"# `gombit generate` regenerates its *.gen.go from the model + gombit field policy.\n"
}

// modelStructTag is the model field tag: gorm, gombit policy, validate
// constraints, and the semantic format or slug pattern.
func modelStructTag(f Field) string {
	base := structTag(f.gormTag(), f.gombitPolicy(), logical.FormatConstraints(f.constraints()))
	var extras []string
	if format := f.openAPIFormat(); format != "" {
		extras = append(extras, `format:"`+format+`"`)
	}
	if pattern := f.semanticPattern(); pattern != "" {
		extras = append(extras, `pattern:"`+pattern+`"`)
	}
	if len(extras) == 0 {
		return base
	}
	extra := strings.Join(extras, " ")
	if base == "" {
		return " `" + extra + "`"
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(base, " `"), "`")
	return " `" + inner + " " + extra + "`"
}

// structTag composes a field's struct tag from its gorm, gombit, and validate
// parts, omitting an empty part (and the whole tag when all are empty).
func structTag(gormPart, gombitPart, validatePart string) string {
	var parts []string
	if gormPart != "" {
		parts = append(parts, `gorm:"`+gormPart+`"`)
	}
	if gombitPart != "" {
		parts = append(parts, `gombit:"`+gombitPart+`"`)
	}
	if validatePart != "" {
		parts = append(parts, `validate:"`+validatePart+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return " `" + strings.Join(parts, " ") + "`"
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

// cmpDecimalJS compares two decimal strings by magnitude. It is emitted when a
// form has a decimal bound so the check does not go through a JavaScript number.
const cmpDecimalJS = `function cmpDecimal(a, b) {
  const norm = (raw) => {
    let t = String(raw).trim();
    let neg = false;
    if (t.startsWith("-")) {
      neg = true;
      t = t.slice(1);
    }
    const parts = t.split(".");
    const ip = (parts[0] || "0").replace(/^0+(?=\d)/, "");
    const fp = (parts[1] || "").replace(/0+$/, "");
    if (ip === "0" && fp === "") neg = false;
    return { neg, ip, fp };
  };
  const left = norm(a);
  const right = norm(b);
  if (left.neg !== right.neg) {
    return left.neg ? -1 : 1;
  }
  const sign = left.neg ? -1 : 1;
  if (left.ip.length !== right.ip.length) {
    return sign * (left.ip.length < right.ip.length ? -1 : 1);
  }
  if (left.ip !== right.ip) {
    return sign * (left.ip < right.ip ? -1 : 1);
  }
  if (left.fp !== right.fp) {
    return sign * (left.fp < right.fp ? -1 : 1);
  }
  return 0;
}

`

func formNeedsDecimalCmp(fields []Field) bool {
	for _, f := range fields {
		if f.Type == FieldDecimal && (f.Min != "" || f.Max != "") {
			return true
		}
	}
	return false
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
	if formNeedsDecimalCmp(ctx.Fields) {
		b.WriteString(cmpDecimalJS)
	}
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
	bodyExpr := "values as CreateBody"
	if jsonNames := jsonFieldNames(ctx.Fields); len(jsonNames) > 0 {
		b.WriteString("    const body: Record<string, unknown> = { ...values };\n")
		b.WriteString(tsParseJSONFields(jsonNames))
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
func jsonFieldNames(fields []Field) []string {
	var names []string
	for _, field := range fields {
		if field.Type == FieldJSON {
			names = append(names, field.JSONName)
		}
	}
	return names
}

// tsJSONValidate keeps the textarea string and returns a message when it is
// not a JSON object or array. Empty is valid here; required handles blank
// required fields, and onSubmit turns a blank optional field into null.
func tsJSONValidate() string {
	return `validate: (value) => { if (value == null || value === "") return true; try { const parsed = JSON.parse(String(value)); if (parsed === null || typeof parsed !== "object") return "must be a JSON object or array"; return true; } catch { return "must be JSON"; } }`
}

func tsParseJSONFields(names []string) string {
	var b strings.Builder
	b.WriteString("    " + tsStringArray(names) + ".forEach((key) => {\n")
	b.WriteString("      const raw = body[key];\n")
	b.WriteString("      if (raw == null || raw === \"\") { body[key] = null; return; }\n")
	b.WriteString("      body[key] = JSON.parse(String(raw));\n")
	b.WriteString("    });\n")
	return b.String()
}

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
	case FieldInt, FieldInt64, FieldUint, FieldFloat:
		if field.Nullable {
			return "number | null"
		}
		return "number"
	case FieldJSON:
		return "string"
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
	if field.Default != "" {
		switch field.Type {
		case FieldInt, FieldInt64, FieldUint, FieldBool:
			return field.Default
		default:
			return strconv.Quote(field.Default)
		}
	}
	switch field.Type {
	case FieldBool:
		return "false"
	case FieldInt, FieldInt64, FieldUint, FieldFloat:
		if field.Nullable {
			return "null"
		}
		return "0"
	case FieldJSON:
		return `""`
	case FieldEnum:
		if len(field.EnumValues) > 0 {
			return `"` + field.EnumValues[0] + `"`
		}
		return `""`
	default:
		return `""`
	}
}

func tsNumberRules(field Field) string {
	var parts []string
	if field.Min != "" {
		parts = append(parts, "min: { value: "+field.Min+", message: \""+field.GoName+" must be at least "+field.Min+"\" }")
	}
	if field.Max != "" {
		parts = append(parts, "max: { value: "+field.Max+", message: \""+field.GoName+" must be at most "+field.Max+"\" }")
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

func htmlNumberAttrs(field Field) string {
	var b strings.Builder
	if field.Min != "" {
		b.WriteString(" min=\"" + field.Min + "\"")
	}
	if field.Max != "" {
		b.WriteString(" max=\"" + field.Max + "\"")
	}
	return b.String()
}

func tsDecimalRules(field Field) string {
	var b strings.Builder
	if field.Required {
		b.WriteString(", required: \"" + field.GoName + " is required\"")
	}
	if field.Min == "" && field.Max == "" {
		return b.String()
	}
	b.WriteString(", validate: (value) => {\n")
	b.WriteString("            if (value == null || value === \"\") return true;\n")
	b.WriteString("            if (!/^-?\\d+(\\.\\d+)?$/.test(String(value))) return \"" + field.GoName + " must be a decimal\";\n")
	if field.Min != "" {
		b.WriteString("            if (cmpDecimal(value, " + strconv.Quote(field.Min) + ") < 0) return \"" + field.GoName + " must be at least " + field.Min + "\";\n")
	}
	if field.Max != "" {
		b.WriteString("            if (cmpDecimal(value, " + strconv.Quote(field.Max) + ") > 0) return \"" + field.GoName + " must be at most " + field.Max + "\";\n")
	}
	b.WriteString("            return true;\n")
	b.WriteString("          }")
	return b.String()
}

func tsTextRegister(field Field) string {
	var parts []string
	if field.blankIsNull() {
		parts = append(parts, `setValueAs: (value) => (value === "" ? null : value)`)
	}
	if field.Required {
		parts = append(parts, "required: \""+field.GoName+" is required\"")
	}
	if field.MaxLength > 0 {
		parts = append(parts, tsCodePointMaxLength(field))
	}
	if pattern := field.formPattern(); pattern != "" {
		parts = append(parts, "pattern: { value: new RegExp("+strconv.Quote(jsFormPattern(pattern))+`, "u"), message: "`+field.GoName+` is invalid" }`)
	}
	if len(parts) == 0 {
		return ""
	}
	return ", { " + strings.Join(parts, ", ") + " }"
}

func tsCodePointMaxLength(field Field) string {
	// React Hook Form's maxLength and the HTML maxlength attribute count
	// UTF-16 code units. Huma's maxLength counts Unicode code points.
	n := strconv.Itoa(field.MaxLength)
	return `validate: (value) => { if (value == null || value === "") return true; if ([...String(value)].length > ` + n + `) return "` + field.GoName + ` is too long"; return true; }`
}

func htmlTextAttrs(field Field) string {
	// No HTML maxlength or pattern. maxlength counts UTF-16 code units, and
	// pattern is anchored. The register options enforce both the same way the
	// request does.
	return ""
}

func renderFormField(field Field) string {
	ident := jsIdent(field.JSONName)
	var b strings.Builder
	b.WriteString("        <label>\n")
	b.WriteString("          " + field.GoName + "\n")
	switch field.Type {
	case FieldText:
		b.WriteString("          <textarea {...register(\"" + field.JSONName + "\"" + tsTextRegister(field) + ")}" + htmlTextAttrs(field) + " />\n")
	case FieldBool:
		b.WriteString("          <input type=\"checkbox\" {...register(\"" + field.JSONName + "\")} />\n")
	case FieldInt, FieldInt64, FieldUint, FieldFloat:
		blank := "0"
		if field.Nullable {
			blank = "null"
		}
		b.WriteString("          <input type=\"number\" {...register(\"" + field.JSONName + "\", { setValueAs: (value) => (value === \"\" ? " + blank + " : Number(value))" + tsNumberRules(field) + " })}" + htmlNumberAttrs(field) + " />\n")
	case FieldDate, FieldUUID:
		// Empty is null. Format date/uuid rejects "".
		inputType := "text"
		if field.Type == FieldDate {
			inputType = "date"
		}
		b.WriteString("          <input type=\"" + inputType + "\" {...register(\"" + field.JSONName + "\", { setValueAs: (value) => (value === \"\" ? null : value)")
		if field.Required {
			b.WriteString(", required: \"" + field.GoName + " is required\"")
		}
		b.WriteString(" })} />\n")
	case FieldJSON:
		// The textarea keeps the raw text. validate reports a parse error
		// without discarding keystrokes. onSubmit parses once.
		b.WriteString("          <textarea {...register(\"" + field.JSONName + "\", { " + tsJSONValidate())
		if field.Required {
			b.WriteString(", required: \"" + field.GoName + " is required\"")
		}
		b.WriteString(" })} />\n")
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
		b.WriteString("          <input type=\"text\" inputMode=\"decimal\" {...register(\"" + field.JSONName + "\", { setValueAs: (value) => (value === \"\" ? null : value)" + tsDecimalRules(field) + " })}" + htmlNumberAttrs(field) + " />\n")
	default:
		inputType := "text"
		switch field.Type {
		case FieldEmail:
			inputType = "email"
		case FieldURL:
			inputType = "url"
		}
		b.WriteString("          <input type=\"" + inputType + "\" {...register(\"" + field.JSONName + "\"" + tsTextRegister(field) + ")}" + htmlTextAttrs(field) + " />\n")
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
	if formNeedsDecimalCmp(ctx.Fields) {
		b.WriteString(cmpDecimalJS)
	}
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
	var timeNames, emptyNullNames []string
	jsonNames := jsonFieldNames(ctx.Fields)
	for _, field := range ctx.Fields {
		switch field.Type {
		case FieldTime:
			timeNames = append(timeNames, field.JSONName)
		case FieldDecimal, FieldDate, FieldUUID:
			emptyNullNames = append(emptyNullNames, field.JSONName)
		default:
			if field.blankIsNull() {
				emptyNullNames = append(emptyNullNames, field.JSONName)
			}
		}
	}
	b.WriteString("  async function onSubmit(values: FormValues) {\n")
	b.WriteString("    setStatus(\"\");\n")
	bodyExpr := "values as CreateBody"
	if len(timeNames) > 0 || len(emptyNullNames) > 0 || len(jsonNames) > 0 {
		b.WriteString("    const body: Record<string, unknown> = { ...values };\n")
		if len(timeNames) > 0 {
			// Local datetime-local -> RFC3339 UTC; empty -> null (optional field).
			b.WriteString("    " + tsStringArray(timeNames) + ".forEach((key) => {\n")
			b.WriteString("      const v = body[key];\n")
			b.WriteString("      body[key] = v == null || v === \"\" ? null : new Date(String(v)).toISOString();\n")
			b.WriteString("    });\n")
		}
		if len(emptyNullNames) > 0 {
			// Empty date, uuid, and decimal strings are null. Format validation
			// rejects "" for date and uuid.
			b.WriteString("    " + tsStringArray(emptyNullNames) + ".forEach((key) => {\n")
			b.WriteString("      if (body[key] == null || body[key] === \"\") body[key] = null;\n")
			b.WriteString("    });\n")
		}
		if len(jsonNames) > 0 {
			b.WriteString(tsParseJSONFields(jsonNames))
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

func muiDecimalBound(field Field) string {
	var lines []string
	lines = append(lines, `if (!/^-?\d+(\.\d+)?$/.test(String(value))) return "`+field.GoName+` must be a decimal";`)
	if field.Min != "" {
		lines = append(lines, `if (cmpDecimal(value, `+strconv.Quote(field.Min)+`) < 0) return "`+field.GoName+` must be at least `+field.Min+`";`)
	}
	if field.Max != "" {
		lines = append(lines, `if (cmpDecimal(value, `+strconv.Quote(field.Max)+`) > 0) return "`+field.GoName+` must be at most `+field.Max+`";`)
	}
	return strings.Join(lines, "\n              ")
}

func muiRules(field Field) string {
	var parts []string
	if field.Required && field.Type != FieldBool {
		parts = append(parts, `required: "`+field.GoName+` is required"`)
	}
	if field.Type == FieldDecimal && (field.Min != "" || field.Max != "") {
		parts = append(parts, `validate: (value) => {
              if (value == null || value === "") return true;
              `+muiDecimalBound(field)+`
              return true;
            }`)
	} else {
		if field.Min != "" {
			parts = append(parts, `min: { value: `+field.Min+`, message: "`+field.GoName+` must be at least `+field.Min+`" }`)
		}
		if field.Max != "" {
			parts = append(parts, `max: { value: `+field.Max+`, message: "`+field.GoName+` must be at most `+field.Max+`" }`)
		}
	}
	if field.MaxLength > 0 {
		parts = append(parts, tsCodePointMaxLength(field))
	}
	if pattern := field.formPattern(); pattern != "" {
		parts = append(parts, `pattern: { value: new RegExp(`+strconv.Quote(jsFormPattern(pattern))+`, "u"), message: "`+field.GoName+` is invalid" }`)
	}
	if field.Type == FieldJSON {
		parts = append(parts, tsJSONValidate())
	}
	if len(parts) == 0 {
		return ""
	}
	return " rules={{ " + strings.Join(parts, ", ") + " }}"
}

func muiBoundAttrs(field Field) string {
	var b strings.Builder
	if field.Min != "" {
		b.WriteString(", min: " + field.Min)
	}
	if field.Max != "" {
		b.WriteString(", max: " + field.Max)
	}
	return b.String()
}

func muiTextSlot(field Field) string {
	var bits []string
	if len(bits) == 0 {
		return ""
	}
	return "                slotProps={{ htmlInput: { " + strings.Join(bits, ", ") + " } }}\n"
}

func renderMUIFormField(field Field) string {
	var b strings.Builder
	rules := muiRules(field)
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
		if extra := muiTextSlot(field); extra != "" {
			b.WriteString(extra)
		}
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              />\n")
	case FieldInt, FieldInt64, FieldUint, FieldFloat:
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		if field.Nullable {
			b.WriteString("                value={field.value ?? \"\"}\n")
		}
		b.WriteString("                type=\"number\"\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		if field.Min != "" || field.Max != "" {
			b.WriteString("                slotProps={{ htmlInput: { ")
			var bits []string
			if field.Min != "" {
				bits = append(bits, "min: "+field.Min)
			}
			if field.Max != "" {
				bits = append(bits, "max: "+field.Max)
			}
			b.WriteString(strings.Join(bits, ", "))
			b.WriteString(" } }}\n")
		}
		b.WriteString("                onChange={(event) => {\n")
		b.WriteString("                  const raw = event.target.value;\n")
		blank := "0"
		if field.Nullable {
			blank = "null"
		}
		b.WriteString("                  field.onChange(raw === \"\" ? " + blank + " : Number(raw));\n")
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
	case FieldDate, FieldUUID:
		inputType := "text"
		if field.Type == FieldDate {
			inputType = "date"
		}
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                value={field.value ?? \"\"}\n")
		b.WriteString("                type=\"" + inputType + "\"\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		if field.Type == FieldDate {
			b.WriteString("                slotProps={{ inputLabel: { shrink: true } }}\n")
		}
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("                onChange={(event) => {\n")
		b.WriteString("                  const raw = event.target.value;\n")
		b.WriteString("                  field.onChange(raw === \"\" ? null : raw);\n")
		b.WriteString("                }}\n")
		b.WriteString("              />\n")
	case FieldJSON:
		// Keep the keystrokes. validate reports a document that is not an
		// object or array, and onSubmit parses the string once.
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                value={field.value ?? \"\"}\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                multiline\n")
		b.WriteString("                minRows={3}\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message ?? \"JSON object or array\"}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("                onChange={(event) => {\n")
		b.WriteString("                  field.onChange(event.target.value);\n")
		b.WriteString("                }}\n")
		b.WriteString("              />\n")
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
		b.WriteString("                slotProps={{ htmlInput: { inputMode: \"decimal\"" + muiBoundAttrs(field) + " } }}\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		b.WriteString("              />\n")
	default:
		inputType := "text"
		switch field.Type {
		case FieldEmail:
			inputType = "email"
		case FieldURL:
			inputType = "url"
		}
		b.WriteString("              <TextField\n")
		b.WriteString("                {...field}\n")
		b.WriteString("                type=\"" + inputType + "\"\n")
		b.WriteString("                label=\"" + field.GoName + "\"\n")
		b.WriteString("                fullWidth\n")
		b.WriteString("                error={!!fieldState.error}\n")
		b.WriteString("                helperText={fieldState.error?.message}\n")
		b.WriteString("                disabled={isSubmitting}\n")
		if extra := muiTextSlot(field); extra != "" {
			b.WriteString(extra)
		}
		b.WriteString("              />\n")
	}
	b.WriteString("            )}\n")
	b.WriteString("          />\n")
	return b.String()
}
