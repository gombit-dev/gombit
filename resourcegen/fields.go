package resourcegen

import (
	"fmt"
	"strconv"
	"strings"
)

// Field is one parsed resource field from the CLI grammar
// name:type[:required][,unique][,index] (design §27 subset).
type Field struct {
	Name     string
	JSONName string
	GoName   string
	Type     FieldType
	GoType   string
	Required bool
	Unique   bool
	Index    bool
	Nullable bool

	// Filterable / Sortable / Searchable opt this field into the generated list
	// handler's declared query surface (issue #260): exact-match ?<field>=,
	// ?ordering=<field> (- prefix for DESC), and ?search= respectively — the same
	// spelling as the admin data plane. belongs_to fields are filterable by
	// default (see isFilterable) so the has_many detail-list case —
	// GET /children?<parent>_id=<id> — works without extra declaration.
	Filterable bool
	Sortable   bool
	Searchable bool

	// Aggregatable opts this numeric field into the generated list handler's
	// server-side aggregate surface (issue #272): ?aggregate=sum:<field>,
	// avg:<field>,min:<field>,max:<field>, computed over the same filtered and
	// searched set as the list, before pagination. Only numeric columns (int,
	// int64, uint, decimal) may opt in (see typeAllowsAggregate).
	Aggregatable bool

	// EnumValues holds the allowed values for FieldEnum, in declared order.
	EnumValues []string
	// Precision/Scale set the decimal(p,s) column for FieldDecimal.
	Precision int
	Scale     int

	// Target is the related model type (PascalCase) for a relation field, e.g.
	// "Engine". TargetPkg is its feature-package name (snake), e.g. "engine".
	Target    string
	TargetPkg string
}

// parseRelationField builds a belongs_to / has_many / many_to_many field from
// its target model. The target is a PascalCase model living in
// internal/<target>/ (imported as <target>.<Target>), or the resource itself
// for a self-referential belongs_to. resourcePkg is the package being
// generated, used to detect same-package targets.
func parseRelationField(name, jsonName, goName string, kind FieldType, target, resourcePkg string) (Field, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Field{}, fmt.Errorf("resourcegen: relation field %q is missing a target model", name)
	}
	targetType := toPascal(target)
	if !isExportedIdent(targetType) {
		return Field{}, fmt.Errorf("resourcegen: relation target %q is not a valid exported Go type", target)
	}
	targetPkg := toSnake(targetType)
	if _, reserved := reservedPackages[targetPkg]; reserved {
		return Field{}, fmt.Errorf("resourcegen: relation target %q maps to reserved package %q", target, targetPkg)
	}
	if _, kw := goKeywords[targetPkg]; kw {
		return Field{}, fmt.Errorf("resourcegen: relation target %q maps to Go keyword %q", target, targetPkg)
	}
	// Self-referential relations are not supported in this milestone. A
	// belongs_to onto the same model would need a nullable (*uint) foreign key so
	// a tree root stores NULL rather than 0 (0 references no row and fails the
	// self-FK); has_many / many_to_many need explicit join / foreign keys. Rather
	// than emit output that cannot insert a root or migrate, reject it here.
	if targetPkg == resourcePkg {
		return Field{}, fmt.Errorf("resourcegen: %s relation %q cannot target the resource itself (self-referential relations are not supported yet; they need a nullable foreign key / explicit join keys)", strings.ToLower(string(kind)), name)
	}
	f := Field{
		Name:      name,
		JSONName:  jsonName,
		GoName:    goName,
		Type:      kind,
		Target:    targetType,
		TargetPkg: targetPkg,
	}
	// The target is always a distinct feature-package (same-package targets are
	// rejected above), qualified as <pkg>.<Type>.
	qualified := targetPkg + "." + targetType
	switch kind {
	case FieldBelongsTo:
		f.GoType = qualified // the association struct field
	case FieldHasMany, FieldManyToMany:
		f.GoType = "[]" + qualified
	}
	return f, nil
}

// reservedJSONKeys returns the lowercased JSON identifiers this field claims in
// the generated output, used for duplicate detection. A belongs_to occupies both
// its own name (the association Go field) and its synthesized foreign key
// (<name>_id), so both must be reserved — otherwise engine:belongs_to:Engine
// plus engine_id:uint would emit the EngineID field twice.
func (f Field) reservedJSONKeys() []string {
	if f.Type == FieldBelongsTo {
		return []string{f.JSONName, f.fkJSONName()}
	}
	return []string{f.JSONName}
}

// fkGoName / fkJSONName are the foreign-key field names for a belongs_to (the
// association is GoName, the FK is GoName+ID).
func (f Field) fkGoName() string   { return f.GoName + "ID" }
func (f Field) fkJSONName() string { return f.JSONName + "_id" }

// dtoGoName / dtoGoType / dtoJSONName name the field as it appears in the
// generated handler DTO. A belongs_to is exposed as its uint foreign key; other
// fields keep their own names. (m2m / has_many are excluded from the DTO via
// inDTO and never reach these.)
func (f Field) dtoGoName() string {
	if f.Type == FieldBelongsTo {
		return f.fkGoName()
	}
	return f.GoName
}

func (f Field) dtoGoType() string {
	if f.Type == FieldBelongsTo {
		return "uint"
	}
	return f.GoType
}

func (f Field) dtoJSONName() string {
	if f.Type == FieldBelongsTo {
		return f.fkJSONName()
	}
	return f.JSONName
}

// dtoHumaTags is the Huma struct tag for the DTO create-input field.
func (f Field) dtoHumaTags() string {
	if f.Type == FieldBelongsTo {
		return fmt.Sprintf(`json:"%s" minimum:"0" doc:"%s"`, f.fkJSONName(), f.fkGoName())
	}
	return f.humaTags()
}

// joinTable is the many2many join-table name for a relation on resourcePkg.
func (f Field) joinTable(resourcePkg string) string { return resourcePkg + "_" + f.JSONName }

// dtoFields projects the fields as they appear in the generated handler DTO and
// frontend: belongs_to becomes its uint foreign key; many_to_many / has_many are
// dropped from the REST DTO (model-only — many_to_many is edited and has_many is
// shown read-only through the admin).
func dtoFields(fields []Field) []Field {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		if !f.inDTO() {
			continue
		}
		if f.Type == FieldBelongsTo {
			out = append(out, Field{
				Name:     f.fkJSONName(),
				JSONName: f.fkJSONName(),
				GoName:   f.fkGoName(),
				Type:     FieldUint,
				GoType:   "uint",
			})
			continue
		}
		out = append(out, f)
	}
	return out
}

// isRelation reports whether the field is a belongs_to / has_many / many_to_many.
func (f Field) isRelation() bool {
	switch f.Type {
	case FieldBelongsTo, FieldHasMany, FieldManyToMany:
		return true
	default:
		return false
	}
}

// inDTO reports whether the field appears in the generated handler DTO. The thin
// generated CRUD exposes belongs_to as its foreign key; many_to_many / has_many
// are model-only — not in the REST DTO — with many_to_many edited and has_many
// shown read-only through the admin.
func (f Field) inDTO() bool {
	return !f.isRelation() || f.Type == FieldBelongsTo
}

// FieldType is a supported scalar in the v0.1 subset.
type FieldType string

const (
	FieldString  FieldType = "string"
	FieldText    FieldType = "text"
	FieldInt     FieldType = "int"
	FieldInt64   FieldType = "int64"
	FieldBool    FieldType = "bool"
	FieldUint    FieldType = "uint"
	FieldDecimal FieldType = "decimal"
	FieldTime    FieldType = "time"
	FieldEnum    FieldType = "enum"

	FieldBelongsTo  FieldType = "belongs_to"
	FieldHasMany    FieldType = "has_many"
	FieldManyToMany FieldType = "many_to_many"
)

// defaultDecimalPrecision / defaultDecimalScale back a bare `decimal` field.
// They match the multi-DB conformance fixture (decimal(19,4)) that already
// passes the SQLite + PostgreSQL + MySQL matrix.
const (
	defaultDecimalPrecision = 19
	defaultDecimalScale     = 4
)

var supportedTypes = []FieldType{
	FieldString, FieldText, FieldInt, FieldInt64, FieldBool, FieldUint,
	FieldDecimal, FieldTime, FieldEnum,
}

// decimalGoType is the fully qualified generated Go type for a decimal field.
const decimalGoType = "types.Decimal"

func parseFields(specs []string, resourcePkg string) ([]Field, error) {
	seen := make(map[string]struct{}, len(specs))
	fields := make([]Field, 0, len(specs))
	for _, spec := range specs {
		field, err := parseField(spec, resourcePkg)
		if err != nil {
			return nil, err
		}
		// Duplicate detection covers every JSON identifier the field emits, not
		// just its grammar token: a belongs_to also claims its <name>_id foreign
		// key (see reservedJSONKeys).
		for _, key := range field.reservedJSONKeys() {
			key = strings.ToLower(key)
			if _, ok := seen[key]; ok {
				return nil, fmt.Errorf("resourcegen: duplicate field %q", key)
			}
			seen[key] = struct{}{}
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func parseField(spec, resourcePkg string) (Field, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Field{}, fmt.Errorf("resourcegen: empty field spec")
	}
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Field{}, fmt.Errorf("resourcegen: field %q must be name:type[:modifiers]", spec)
	}
	name := strings.TrimSpace(parts[0])
	// Keep original case for the type token: enum(...) values are
	// case-sensitive. The base keyword is matched case-insensitively.
	typeToken := strings.TrimSpace(parts[1])
	if name == "" || typeToken == "" {
		return Field{}, fmt.Errorf("resourcegen: field %q must be name:type[:modifiers]", spec)
	}

	jsonName := toSnake(name)
	goName := toPascal(name)
	if jsonName == "" || !isExportedIdent(goName) {
		return Field{}, fmt.Errorf("resourcegen: field name %q is not a valid identifier", name)
	}
	if _, reserved := reservedFields[jsonName]; reserved {
		return Field{}, fmt.Errorf("resourcegen: field %q conflicts with gorm.Model", jsonName)
	}
	if _, reserved := reservedQueryFields[jsonName]; reserved {
		return Field{}, fmt.Errorf("resourcegen: field %q is reserved for the list-query params (page, per_page, search, ordering, aggregate); rename it", jsonName)
	}

	// Relations (name:kind:Target) use parts[2] as the target model, not
	// modifiers.
	switch FieldType(strings.ToLower(typeToken)) {
	case FieldBelongsTo, FieldHasMany, FieldManyToMany:
		if len(parts) != 3 {
			return Field{}, fmt.Errorf("resourcegen: relation field %q must be name:%s:Target", spec, strings.ToLower(typeToken))
		}
		return parseRelationField(name, jsonName, goName, FieldType(strings.ToLower(typeToken)), parts[2], resourcePkg)
	}

	field := Field{
		Name:     name,
		JSONName: jsonName,
		GoName:   goName,
	}
	if err := applyType(&field, typeToken); err != nil {
		return Field{}, err
	}
	if len(parts) == 3 {
		if err := applyModifiers(&field, parts[2]); err != nil {
			return Field{}, err
		}
	}
	// time.Time / types.Decimal are value types Huma cannot leave empty: an
	// optional field submits JSON null, and a non-pointer time/decimal rejects
	// null (and empty string). Optional (non-required) time/decimal fields
	// therefore become pointers on both the model and the DTO so an empty value
	// round-trips. See #222 review.
	if !field.Required && (field.Type == FieldTime || field.Type == FieldDecimal) {
		field.GoType = "*" + field.GoType
	}
	return field, nil
}

// applyType parses the type token (which may carry arguments, e.g.
// `decimal(19,4)` or `enum(a,b,c)`) and fills field.Type/GoType and any
// type-specific data (enum values, decimal precision/scale).
func applyType(field *Field, token string) error {
	base, args, hasArgs := splitTypeArgs(token)
	switch strings.ToLower(base) {
	case "string":
		field.Type, field.GoType = FieldString, "string"
	case "text":
		field.Type, field.GoType = FieldText, "string"
	case "int":
		field.Type, field.GoType = FieldInt, "int"
	case "int64":
		field.Type, field.GoType = FieldInt64, "int64"
	case "bool":
		field.Type, field.GoType = FieldBool, "bool"
	case "uint":
		field.Type, field.GoType = FieldUint, "uint"
	case "time":
		field.Type, field.GoType = FieldTime, "time.Time"
	case "decimal":
		field.Type, field.GoType = FieldDecimal, decimalGoType
		field.Precision, field.Scale = defaultDecimalPrecision, defaultDecimalScale
		if hasArgs {
			p, s, err := parseDecimalArgs(args)
			if err != nil {
				return err
			}
			field.Precision, field.Scale = p, s
		}
	case "enum":
		if !hasArgs {
			return fmt.Errorf("resourcegen: enum field %q needs values, e.g. status:enum(draft,published)", field.JSONName)
		}
		values, err := parseEnumValues(args)
		if err != nil {
			return err
		}
		field.Type, field.GoType = FieldEnum, "string"
		field.EnumValues = values
	default:
		names := make([]string, 0, len(supportedTypes))
		for _, item := range supportedTypes {
			names = append(names, string(item))
		}
		return fmt.Errorf("resourcegen: unknown type %q (supported: %s)", token, strings.Join(names, ", "))
	}
	if hasArgs && field.Type != FieldDecimal && field.Type != FieldEnum {
		return fmt.Errorf("resourcegen: type %q does not take arguments", base)
	}
	return nil
}

// splitTypeArgs splits `enum(a,b)` into ("enum", "a,b", true) and `int` into
// ("int", "", false).
func splitTypeArgs(token string) (base, args string, hasArgs bool) {
	open := strings.IndexByte(token, '(')
	if open < 0 {
		return token, "", false
	}
	if !strings.HasSuffix(token, ")") {
		return token, "", false
	}
	return strings.TrimSpace(token[:open]), token[open+1 : len(token)-1], true
}

func parseDecimalArgs(args string) (precision, scale int, err error) {
	fields := strings.Split(args, ",")
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("resourcegen: decimal precision must be decimal(precision,scale), got decimal(%s)", args)
	}
	p, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("resourcegen: decimal precision %q is not an integer", fields[0])
	}
	s, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("resourcegen: decimal scale %q is not an integer", fields[1])
	}
	if p <= 0 || s < 0 || s > p {
		return 0, 0, fmt.Errorf("resourcegen: invalid decimal(%d,%d): need precision > 0 and 0 <= scale <= precision", p, s)
	}
	return p, s, nil
}

func parseEnumValues(args string) ([]string, error) {
	raw := strings.Split(args, ",")
	values := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, fmt.Errorf("resourcegen: enum has an empty value")
		}
		// Values land in a Go struct tag and a TS union literal; keep them to
		// a safe, unambiguous character set.
		for _, r := range v {
			if r == '"' || r == '`' || r == '\\' {
				return nil, fmt.Errorf("resourcegen: enum value %q contains an unsupported character", v)
			}
		}
		if _, dup := seen[v]; dup {
			return nil, fmt.Errorf("resourcegen: duplicate enum value %q", v)
		}
		seen[v] = struct{}{}
		values = append(values, v)
	}
	return values, nil
}

func applyModifiers(field *Field, raw string) error {
	for _, part := range strings.Split(raw, ",") {
		mod := strings.ToLower(strings.TrimSpace(part))
		if mod == "" {
			continue
		}
		switch {
		case mod == "required":
			field.Required = true
		case mod == "unique":
			field.Unique = true
		case mod == "index":
			field.Index = true
		case mod == "nullable":
			field.Nullable = true
		case mod == "filterable":
			field.Filterable = true
		case mod == "sortable":
			field.Sortable = true
		case mod == "searchable":
			field.Searchable = true
		case mod == "aggregatable":
			field.Aggregatable = true
		case strings.HasPrefix(mod, "default="), strings.HasPrefix(mod, "min="), strings.HasPrefix(mod, "max="), strings.HasPrefix(mod, "references="):
			return fmt.Errorf("resourcegen: modifier %q is not supported in this milestone (supported: required, unique, index, nullable, filterable, sortable, searchable, aggregatable)", mod)
		default:
			return fmt.Errorf("resourcegen: unknown modifier %q (supported: required, unique, index, nullable, filterable, sortable, searchable, aggregatable)", mod)
		}
	}
	if field.Required && field.Nullable {
		return fmt.Errorf("resourcegen: field %q cannot be both required and nullable", field.JSONName)
	}
	if field.Filterable && !field.typeAllowsFilter() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be filterable (supported: string, int, int64, uint, bool, enum, belongs_to)", field.JSONName, field.Type)
	}
	if field.Searchable && !field.typeAllowsSearch() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be searchable (supported: string, text, enum)", field.JSONName, field.Type)
	}
	if field.Sortable && !field.typeAllowsSort() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be sortable", field.JSONName, field.Type)
	}
	if field.Aggregatable && !field.typeAllowsAggregate() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be aggregatable (supported: int, int64, uint, decimal)", field.JSONName, field.Type)
	}
	return nil
}

// typeAllowsFilter reports whether an exact-match filter query param can be
// generated for this field's type. Exact-match on decimal/time is fiddly to
// coerce and rarely useful (ranges come later, #260), and text columns are for
// search, not equality; both are excluded.
func (f Field) typeAllowsFilter() bool {
	switch f.Type {
	case FieldString, FieldInt, FieldInt64, FieldUint, FieldBool, FieldEnum, FieldBelongsTo:
		return true
	default:
		return false
	}
}

// typeAllowsSearch reports whether the field is a text-like column ?search= can LIKE.
func (f Field) typeAllowsSearch() bool {
	switch f.Type {
	case FieldString, FieldText, FieldEnum:
		return true
	default:
		return false
	}
}

// typeAllowsSort reports whether the field maps to a single orderable column.
// Every scalar and the belongs_to foreign key qualifies; has_many / many_to_many
// (multi-row associations) do not.
func (f Field) typeAllowsSort() bool {
	switch f.Type {
	case FieldHasMany, FieldManyToMany:
		return false
	default:
		return true
	}
}

// typeAllowsAggregate reports whether SUM/AVG/MIN/MAX can be applied to this
// field's column. Only the numeric scalars qualify (issue #272): int, int64,
// uint, decimal. Booleans, text, time, enum and relations are excluded — a SUM
// over them is meaningless or driver-dependent.
func (f Field) typeAllowsAggregate() bool {
	switch f.Type {
	case FieldInt, FieldInt64, FieldUint, FieldDecimal:
		return true
	default:
		return false
	}
}

// isFilterable reports whether the generated list handler exposes an exact-match
// filter for this field. belongs_to foreign keys are filterable by default so
// the has_many detail-list case (GET /children?<parent>_id=<id>, issue #260 /
// Forge #53) needs no extra declaration; every other field opts in.
func (f Field) isFilterable() bool {
	return f.Filterable || f.Type == FieldBelongsTo
}

// filterColumn / searchColumn / sortColumn / aggregateColumn are the DB column
// names the list query references. For our naming they equal the DTO JSON name
// (toSnake of the Go field), which is what GORM's default naming strategy
// derives for the model column — belongs_to resolves to its <name>_id foreign
// key. aggregateColumn is only used for numeric scalars, never a relation.
func (f Field) filterColumn() string    { return f.dtoJSONName() }
func (f Field) searchColumn() string    { return f.JSONName }
func (f Field) sortColumn() string      { return f.dtoJSONName() }
func (f Field) aggregateColumn() string { return f.dtoJSONName() }

// filterInputField names the field on the generated list-input struct. Filters
// are string query params (Huma has no optional/pointer query params, so empty
// string is the "absent" signal); the value is coerced server-side by kind.
func (f Field) filterInputField() string { return f.dtoGoName() }

// filterKindExpr is the database.FilterKind the generated handler passes to
// database.FilterEq so it coerces the raw filter string to the column's type.
func (f Field) filterKindExpr() string {
	switch f.Type {
	case FieldInt:
		return "database.FilterInt"
	case FieldInt64:
		return "database.FilterInt64"
	case FieldUint, FieldBelongsTo:
		return "database.FilterUint"
	case FieldBool:
		return "database.FilterBool"
	default: // FieldString, FieldEnum
		return "database.FilterString"
	}
}

// filterQueryTag builds the Huma struct tag for a filter query param: the query
// name, an enum constraint for bool (true/false) and enum columns so Huma
// rejects bad values before the handler, and a doc string.
func (f Field) filterQueryTag() string {
	tag := `query:"` + f.filterColumn() + `"`
	switch f.Type {
	case FieldEnum:
		tag += ` enum:"` + strings.Join(f.EnumValues, ",") + `"`
	case FieldBool:
		tag += ` enum:"true,false"`
	}
	tag += ` doc:"Filter by ` + f.filterInputField() + ` (exact match)"`
	return tag
}

func (f Field) gormTag() string {
	var parts []string
	switch f.Type {
	case FieldString:
		parts = append(parts, "size:255")
	case FieldText:
		parts = append(parts, "type:text")
	case FieldEnum:
		parts = append(parts, "size:"+strconv.Itoa(enumColumnSize(f.EnumValues)))
	case FieldDecimal:
		parts = append(parts, fmt.Sprintf("type:decimal(%d,%d)", f.Precision, f.Scale))
	}
	if f.Required && !f.Nullable {
		parts = append(parts, "not null")
	}
	switch {
	case f.Unique:
		parts = append(parts, "uniqueIndex")
	case f.Index:
		parts = append(parts, "index")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ";")
}

func (f Field) humaTags() string {
	var parts []string
	parts = append(parts, fmt.Sprintf(`json:"%s"`, f.JSONName))
	switch f.Type {
	case FieldString:
		if f.Required {
			parts = append(parts, `minLength:"1"`)
		}
		parts = append(parts, `maxLength:"255"`)
	case FieldText:
		if f.Required {
			parts = append(parts, `minLength:"1"`)
		}
	case FieldUint:
		parts = append(parts, `minimum:"0"`)
	case FieldEnum:
		parts = append(parts, fmt.Sprintf(`enum:"%s"`, strings.Join(f.EnumValues, ",")))
	}
	parts = append(parts, fmt.Sprintf(`doc:"%s"`, f.GoName))
	return strings.Join(parts, " ")
}

// enumColumnSize sizes the varchar column to hold the longest allowed value,
// with headroom so a later value addition rarely needs a column widen.
func enumColumnSize(values []string) int {
	longest := 0
	for _, v := range values {
		if len(v) > longest {
			longest = len(v)
		}
	}
	size := longest + 16
	if size < 32 {
		size = 32
	}
	return size
}
