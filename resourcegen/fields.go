package resourcegen

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	logical "github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/types"
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

	// Declarative constraints (MODEL-3). Empty strings and a zero MaxLength
	// mean unset. Default is the raw token, without SQL quotes.
	Min       string
	Max       string
	MaxLength int
	Pattern   string
	Default   string

	// Target is the related model type (PascalCase) for a relation field, e.g.
	// "Engine". TargetPkg is its feature-package name (snake), e.g. "engine".
	// OnDelete is the GORM OnDelete clause (RESTRICT, CASCADE, SET NULL).
	// Self is a relation whose target is the resource being generated.
	Target    string
	TargetPkg string
	OnDelete  string
	Self      bool
}

// parseRelationField builds a belongs_to / has_many / many_to_many / one_to_one
// field from its target model. The target is a PascalCase model living in
// internal/<target>/ (imported as <target>.<Target>), or the resource itself
// for a nullable self-referential belongs_to or one_to_one. Modifiers after
// the target are comma-separated: nullable and on_delete=restrict|cascade|set_null.
// resourcePkg is the package being generated, used to detect same-package targets.
func parseRelationField(name, jsonName, goName string, kind FieldType, rawTarget, resourcePkg string) (Field, error) {
	target, modRaw, _ := strings.Cut(rawTarget, ",")
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
	f := Field{
		Name:      name,
		JSONName:  jsonName,
		GoName:    goName,
		Type:      kind,
		Target:    targetType,
		TargetPkg: targetPkg,
		OnDelete:  "RESTRICT",
		Self:      targetPkg == resourcePkg,
	}
	if strings.TrimSpace(modRaw) != "" {
		if err := applyRelationModifiers(&f, modRaw); err != nil {
			return Field{}, err
		}
	}
	if f.OnDelete == "SET NULL" && !f.Nullable {
		return Field{}, fmt.Errorf("resourcegen: %s relation %q on_delete=set_null requires nullable", strings.ToLower(string(kind)), name)
	}
	// A self-referential belongs_to or one_to_one needs a nullable (*uint)
	// foreign key so a tree root stores NULL rather than 0. has_many and
	// many_to_many onto the same model still need explicit join keys.
	if f.Self && (kind == FieldHasMany || kind == FieldManyToMany) {
		return Field{}, fmt.Errorf("resourcegen: %s relation %q cannot target the resource itself (self-referential has_many and many_to_many need explicit join keys)", strings.ToLower(string(kind)), name)
	}
	if f.Self && !f.Nullable {
		return Field{}, fmt.Errorf("resourcegen: %s relation %q cannot target the resource itself without nullable (a tree root must store NULL, not 0)", strings.ToLower(string(kind)), name)
	}
	qualified := targetPkg + "." + targetType
	if f.Self {
		// A struct cannot contain a field of its own type. The association
		// is a pointer; the foreign key *uint is the nullable column.
		qualified = "*" + targetType
	}
	switch kind {
	case FieldBelongsTo, FieldOneToOne:
		f.GoType = qualified
	case FieldHasMany, FieldManyToMany:
		f.GoType = "[]" + qualified
	}
	return f, nil
}

func applyRelationModifiers(field *Field, raw string) error {
	// The foreign key, and therefore nullable / on_delete, lives on this model
	// only for belongs_to and one_to_one. has_many and many_to_many would drop
	// the modifier, so they are rejected instead of silently ignored.
	if field.Type != FieldBelongsTo && field.Type != FieldOneToOne {
		return fmt.Errorf("resourcegen: %s relation %q does not accept modifiers (nullable and on_delete apply to belongs_to and one_to_one; the foreign key lives on the other model)", strings.ToLower(string(field.Type)), field.JSONName)
	}
	for _, part := range strings.Split(raw, ",") {
		mod := strings.TrimSpace(part)
		if mod == "" {
			continue
		}
		key, val, hasVal := strings.Cut(mod, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch {
		case key == "nullable" && !hasVal:
			field.Nullable = true
		case key == "on_delete" && hasVal:
			switch strings.ToLower(val) {
			case "restrict":
				field.OnDelete = "RESTRICT"
			case "cascade":
				field.OnDelete = "CASCADE"
			case "set_null":
				field.OnDelete = "SET NULL"
			default:
				return fmt.Errorf("resourcegen: relation %q on_delete must be restrict, cascade, or set_null", field.JSONName)
			}
		default:
			return fmt.Errorf("resourcegen: relation %q has unknown modifier %q (supported: nullable, on_delete=restrict|cascade|set_null)", field.JSONName, mod)
		}
	}
	return nil
}

// reservedJSONKeys returns the lowercased JSON identifiers this field claims in
// the generated output, used for duplicate detection. A belongs_to occupies both
// its own name (the association Go field) and its synthesized foreign key
// (<name>_id), so both must be reserved — otherwise engine:belongs_to:Engine
// plus engine_id:uint would emit the EngineID field twice.
func (f Field) reservedJSONKeys() []string {
	if f.Type == FieldBelongsTo || f.Type == FieldOneToOne {
		return []string{f.JSONName, f.fkJSONName()}
	}
	return []string{f.JSONName}
}

// fkGoName / fkJSONName are the foreign-key field names for a belongs_to (the
// association is GoName, the FK is GoName+ID).
func (f Field) fkGoName() string   { return f.GoName + "ID" }
func (f Field) fkJSONName() string { return f.JSONName + "_id" }

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
		if f.Type == FieldBelongsTo || f.Type == FieldOneToOne {
			goType := "uint"
			if f.Nullable {
				goType = "*uint"
			}
			out = append(out, Field{
				Name:     f.fkJSONName(),
				JSONName: f.fkJSONName(),
				GoName:   f.fkGoName(),
				Type:     FieldUint,
				GoType:   goType,
				Nullable: f.Nullable,
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
	case FieldBelongsTo, FieldHasMany, FieldManyToMany, FieldOneToOne:
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
	return !f.isRelation() || f.Type == FieldBelongsTo || f.Type == FieldOneToOne
}

// FieldType is the generator's projection of a logical field kind. Scalar
// values are field.Kind strings. Relation values are field.RelationKind
// strings. Both sets are defined in package field; these constants exist so
// call sites can switch on them.
type FieldType string

const (
	FieldString  FieldType = FieldType(logical.String)
	FieldText    FieldType = FieldType(logical.Text)
	FieldInt     FieldType = FieldType(logical.Integer)
	FieldInt64   FieldType = FieldType(logical.Integer64)
	FieldBool    FieldType = FieldType(logical.Boolean)
	FieldUint    FieldType = FieldType(logical.Unsigned)
	FieldDecimal FieldType = FieldType(logical.Decimal)
	FieldTime    FieldType = FieldType(logical.DateTime)
	FieldDate    FieldType = FieldType(logical.Date)
	FieldFloat   FieldType = FieldType(logical.Float)
	FieldUUID    FieldType = FieldType(logical.UUID)
	FieldJSON    FieldType = FieldType(logical.JSON)
	FieldEmail   FieldType = FieldType(logical.Email)
	FieldURL     FieldType = FieldType(logical.URL)
	FieldSlug    FieldType = FieldType(logical.Slug)
	FieldIP      FieldType = FieldType(logical.IP)
	FieldEnum    FieldType = FieldType(logical.Enum)

	FieldBelongsTo  FieldType = FieldType(logical.RelBelongsTo)
	FieldHasMany    FieldType = FieldType(logical.RelHasMany)
	FieldManyToMany FieldType = FieldType(logical.RelManyToMany)
	FieldOneToOne   FieldType = FieldType(logical.RelOneToOne)
)

// defaultDecimalPrecision / defaultDecimalScale back a bare `decimal` field.
// They match the multi-DB conformance fixture (decimal(19,4)) that already
// passes the SQLite + PostgreSQL + MySQL matrix.
const (
	defaultDecimalPrecision = 19
	defaultDecimalScale     = 4
)

// logicalKind / relationKind project a parsed field back onto the shared
// vocabulary. Relation field types are relation kinds, not scalar kinds.
func (f Field) logicalKind() logical.Kind {
	switch f.Type {
	case FieldBelongsTo, FieldHasMany, FieldManyToMany, FieldOneToOne:
		return logical.Relation
	default:
		return logical.Kind(f.Type)
	}
}

func (f Field) relationKind() logical.RelationKind {
	switch f.Type {
	case FieldBelongsTo:
		return logical.RelBelongsTo
	case FieldHasMany:
		return logical.RelHasMany
	case FieldManyToMany:
		return logical.RelManyToMany
	case FieldOneToOne:
		return logical.RelOneToOne
	default:
		return ""
	}
}

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
	// SplitN keeps a modifier value that contains colons, such as
	// default=https://example.com.
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 {
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
	// modifiers. The token comes from ParseCLI plus relationCaps, not from a
	// parallel list of FieldType constants.
	if kind, rel, ok := logical.ParseCLI(typeToken); ok && rel != "" {
		if len(parts) != 3 {
			return Field{}, fmt.Errorf("resourcegen: relation field %q must be name:%s:Target", spec, rel)
		}
		relSpec, _ := logical.Lookup(kind)
		if !relSpec.GeneratorReady {
			return Field{}, fmt.Errorf("resourcegen: type %q is in the field vocabulary but is not generated yet (see docs/fields.md)", rel)
		}
		return parseRelationField(name, jsonName, goName, FieldType(rel), parts[2], resourcePkg)
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
	// Optional time.Time becomes a pointer because that is the case Huma treats
	// as a nullable string. Date, decimal, and uuid are also pointers so a
	// blank value is SQL NULL and JSON null on the model. OpenAPI nullability
	// for date and uuid is the DTO tag (nullable:"true"), not the pointer:
	// Date has no SchemaProvider, and uuid.UUID is an array Huma unwraps
	// before it records the pointer.
	// Optional time, date, decimal, and uuid are pointers so a blank is SQL
	// NULL. Email, url, slug, and ip are the same: format and pattern reject
	// "", so the blank has to be null. A string or text regex has the same
	// shape, and shares this path.
	if !field.Required && field.blankIsNull() {
		field.GoType = "*" + field.GoType
	}
	// Optional JSON is a different type, not a pointer. types.JSON.Schema is
	// anyOf object|array, and Huma rejects null in anyOf before it honors a
	// nullable tag or a pointer. types.NullJSON is that schema plus null.
	if field.Type == FieldJSON && !field.Required {
		field.GoType = "types.NullJSON"
	}
	return field, nil
}

// applyType parses the type token (which may carry arguments, e.g.
// `decimal(19,4)` or `enum(a,b,c)`) and fills field.Type/GoType and any
// type-specific data (enum values, decimal precision/scale).
func applyType(field *Field, token string) error {
	base, args, hasArgs := splitTypeArgs(token)
	kind, rel, ok := logical.ParseCLI(base)
	if ok && rel != "" {
		return fmt.Errorf("resourcegen: relation type %q must be name:%s:Target", strings.ToLower(base), rel)
	}
	if !ok {
		return fmt.Errorf("resourcegen: unknown type %q (supported: %s)", token, strings.Join(logical.PreferredGeneratorTokens(), ", "))
	}
	spec, _ := logical.Lookup(kind)
	if !spec.GeneratorReady {
		return fmt.Errorf("resourcegen: type %q is in the field vocabulary but is not generated yet (see docs/fields.md)", strings.ToLower(base))
	}
	field.Type = FieldType(kind)
	field.GoType = spec.GoType
	switch kind {
	case logical.Decimal:
		field.Precision, field.Scale = defaultDecimalPrecision, defaultDecimalScale
		if hasArgs {
			p, s, err := parseDecimalArgs(args)
			if err != nil {
				return err
			}
			field.Precision, field.Scale = p, s
		}
	case logical.Enum:
		if !hasArgs {
			return fmt.Errorf("resourcegen: enum field %q needs values, e.g. status:enum(draft,published)", field.JSONName)
		}
		values, err := parseEnumValues(args)
		if err != nil {
			return err
		}
		field.EnumValues = values
	}
	if hasArgs && kind != logical.Decimal && kind != logical.Enum {
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
			if r == '"' || r == '`' || r == '\\' || r == ';' {
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
	const supported = "required, unique, index, nullable, filterable, sortable, searchable, aggregatable, default=, min=, max=, max_length=, regex="
	for _, part := range strings.Split(raw, ",") {
		mod := strings.TrimSpace(part)
		if mod == "" {
			continue
		}
		key, val, hasVal := strings.Cut(mod, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch {
		case key == "required" && !hasVal:
			field.Required = true
		case key == "unique" && !hasVal:
			field.Unique = true
		case key == "index" && !hasVal:
			field.Index = true
		case key == "nullable" && !hasVal:
			field.Nullable = true
		case key == "filterable" && !hasVal:
			field.Filterable = true
		case key == "sortable" && !hasVal:
			field.Sortable = true
		case key == "searchable" && !hasVal:
			field.Searchable = true
		case key == "aggregatable" && !hasVal:
			field.Aggregatable = true
		case key == "default" && hasVal:
			if val == "" {
				return fmt.Errorf("resourcegen: field %q default must be a value", field.JSONName)
			}
			field.Default = val
		case key == "min" && hasVal:
			field.Min = val
		case key == "max" && hasVal:
			field.Max = val
		case key == "max_length" && hasVal:
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return fmt.Errorf("resourcegen: field %q max_length %q must be a positive integer", field.JSONName, val)
			}
			field.MaxLength = n
		case key == "regex" && hasVal:
			if val == "" {
				return fmt.Errorf("resourcegen: field %q regex must be a pattern", field.JSONName)
			}
			if err := portablePattern(val); err != nil {
				return fmt.Errorf("resourcegen: field %q regex: %w", field.JSONName, err)
			}
			field.Pattern = val
		case key == "references":
			return fmt.Errorf("resourcegen: modifier %q is not supported in this milestone (supported: %s)", mod, supported)
		default:
			return fmt.Errorf("resourcegen: unknown modifier %q (supported: %s)", mod, supported)
		}
	}
	if err := validateConstraints(field); err != nil {
		return err
	}
	if field.Required && field.Nullable {
		return fmt.Errorf("resourcegen: field %q cannot be both required and nullable", field.JSONName)
	}
	if field.Filterable && !field.typeAllowsFilter() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be filterable (supported: string, int, int64, uint, bool, enum, belongs_to)", field.JSONName, field.Type)
	}
	if field.Searchable && !field.typeAllowsSearch() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be searchable (supported: string, text, enum, email, slug)", field.JSONName, field.Type)
	}
	if field.Sortable && !field.typeAllowsSort() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be sortable", field.JSONName, field.Type)
	}
	if field.Aggregatable && !field.typeAllowsAggregate() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot be aggregatable (supported: int, int64, uint, decimal)", field.JSONName, field.Type)
	}
	return nil
}

func validateConstraints(field *Field) error {
	if field.Min != "" || field.Max != "" {
		if !constraintNumeric(field.Type) {
			return fmt.Errorf("resourcegen: field %q is %s and cannot take min or max (supported: int, int64, uint, decimal)", field.JSONName, field.Type)
		}
		if field.Min != "" {
			if err := checkNumber(field, "min", field.Min); err != nil {
				return err
			}
		}
		if field.Max != "" {
			if err := checkNumber(field, "max", field.Max); err != nil {
				return err
			}
		}
		if field.Min != "" && field.Max != "" {
			cmp, err := compareNumbers(field, field.Min, field.Max)
			if err != nil {
				return err
			}
			if cmp > 0 {
				return fmt.Errorf("resourcegen: field %q min %s is greater than max %s", field.JSONName, field.Min, field.Max)
			}
		}
		if _, reserved := sqlCheckReserved[field.JSONName]; reserved {
			return fmt.Errorf("resourcegen: field %q is reserved in SQLite, PostgreSQL, or MySQL, so a check constraint cannot name it", field.JSONName)
		}
	}
	if field.MaxLength > 0 && !field.takesMaxLength() {
		return fmt.Errorf("resourcegen: field %q is %s and cannot take max_length (supported: string, email, url, slug, ip)", field.JSONName, field.Type)
	}
	if field.Pattern != "" && field.Type != FieldString && field.Type != FieldText {
		return fmt.Errorf("resourcegen: field %q is %s and cannot take regex (supported: string, text)", field.JSONName, field.Type)
	}
	if strings.ContainsAny(field.Default, "`;\"") || strings.ContainsAny(field.Pattern, "`;\"") {
		return fmt.Errorf("resourcegen: field %q default and regex cannot contain quotes, backticks, or semicolons", field.JSONName)
	}
	if field.Default != "" {
		if err := checkDefault(field); err != nil {
			return err
		}
	}
	return nil
}

func constraintNumeric(t FieldType) bool {
	switch t {
	case FieldInt, FieldInt64, FieldUint, FieldDecimal:
		return true
	default:
		return false
	}
}

func checkNumber(field *Field, name, raw string) error {
	if field.Type == FieldUint {
		if _, err := strconv.ParseUint(raw, 10, 64); err != nil {
			return fmt.Errorf("resourcegen: field %q %s %q must be an unsigned integer", field.JSONName, name, raw)
		}
		return exactNumberToken(field, name, raw)
	}
	if field.Type == FieldDecimal {
		if !decimalSpelling.MatchString(raw) {
			return fmt.Errorf("resourcegen: field %q %s %q must match the decimal schema", field.JSONName, name, raw)
		}
		return decimalFitsColumn(field, name, raw)
	}
	if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
		return fmt.Errorf("resourcegen: field %q %s %q must be an integer", field.JSONName, name, raw)
	}
	return exactNumberToken(field, name, raw)
}

// decimalFitsColumn rejects a token decimal(p,s) would round or overflow.
// The create body compares the exact string, then PostgreSQL and MySQL round
// the assignment to Scale before the CHECK, which turns an accepted body into
// a 500.
func decimalFitsColumn(field *Field, name, raw string) error {
	body := strings.TrimPrefix(raw, "-")
	whole, frac, _ := strings.Cut(body, ".")
	whole = strings.TrimLeft(whole, "0")
	if len(whole) > field.Precision-field.Scale || len(frac) > field.Scale {
		return fmt.Errorf("resourcegen: field %q %s %q does not fit decimal(%d,%d)", field.JSONName, name, raw, field.Precision, field.Scale)
	}
	return nil
}

// maxExactInteger is the last integer where a float64 comparison and an integer
// comparison accept the same values. 2^53 round-trips, but the next integer
// collapses onto it: Huma's maximum check sees the float and the struct keeps
// the int, so the CHECK then fails as a 500.
const maxExactInteger = 1<<53 - 1

// exactNumberToken rejects an integer token the request and the form cannot
// enforce as the same integer the SQL check uses. The token must round-trip
// through ParseFloat, and it must sit inside ±(2^53−1).
func exactNumberToken(field *Field, name, raw string) error {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(f, 0) || math.Trunc(f) != f || strconv.FormatFloat(f, 'f', -1, 64) != raw || f > maxExactInteger || f < -maxExactInteger {
		return fmt.Errorf("resourcegen: field %q %s %q must be an integer in ±(2^53-1); the request and the form compare it as a number", field.JSONName, name, raw)
	}
	return nil
}

func checkDefault(field *Field) error {
	switch field.Type {
	case FieldEnum:
		for _, v := range field.EnumValues {
			if v == field.Default {
				return nil
			}
		}
		return fmt.Errorf("resourcegen: field %q default %q is not an enum value", field.JSONName, field.Default)
	case FieldBool:
		if field.Default != "true" && field.Default != "false" {
			return fmt.Errorf("resourcegen: field %q default %q must be true or false", field.JSONName, field.Default)
		}
	case FieldInt, FieldInt64, FieldUint, FieldDecimal:
		if err := checkNumber(field, "default", field.Default); err != nil {
			return err
		}
		if err := defaultInRange(field); err != nil {
			return err
		}
	case FieldString, FieldText, FieldEmail, FieldURL, FieldSlug, FieldIP:
		if field.MaxLength > 0 && utf8.RuneCountInString(field.Default) > field.MaxLength {
			return fmt.Errorf("resourcegen: field %q default is longer than max_length", field.JSONName)
		}
		if field.Pattern != "" {
			re, err := regexp.Compile(field.Pattern)
			if err != nil {
				return err
			}
			if !re.MatchString(field.Default) {
				return fmt.Errorf("resourcegen: field %q default %q does not match regex", field.JSONName, field.Default)
			}
		}
		if pattern := field.semanticPattern(); pattern != "" && field.Pattern == "" {
			if !regexp.MustCompile(pattern).MatchString(field.Default) {
				return fmt.Errorf("resourcegen: field %q default %q does not match pattern", field.JSONName, field.Default)
			}
		}
		if format := field.openAPIFormat(); format != "" && !formatAccepts(format, field.Default) {
			return fmt.Errorf("resourcegen: field %q default %q is not a valid %s", field.JSONName, field.Default, format)
		}
	default:
		return fmt.Errorf("resourcegen: field %q is %s and cannot take default", field.JSONName, field.Type)
	}
	return nil
}

func defaultInRange(field *Field) error {
	if field.Min != "" {
		cmp, err := compareNumbers(field, field.Default, field.Min)
		if err != nil {
			return err
		}
		if cmp < 0 {
			return fmt.Errorf("resourcegen: field %q default %s is below min %s", field.JSONName, field.Default, field.Min)
		}
	}
	if field.Max != "" {
		cmp, err := compareNumbers(field, field.Default, field.Max)
		if err != nil {
			return err
		}
		if cmp > 0 {
			return fmt.Errorf("resourcegen: field %q default %s is above max %s", field.JSONName, field.Default, field.Max)
		}
	}
	return nil
}

// compareNumbers orders a and b as integers or as decimals. checkNumber has
// already required an integer token to lie inside ±(2^53−1), where Huma's
// float64 minimum/maximum and the SQL check accept the same integers, and a
// decimal token to match the decimal schema.
func compareNumbers(field *Field, a, b string) (int, error) {
	switch field.Type {
	case FieldUint:
		av, err := strconv.ParseUint(a, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("resourcegen: field %q value %q must be an unsigned integer", field.JSONName, a)
		}
		bv, err := strconv.ParseUint(b, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("resourcegen: field %q value %q must be an unsigned integer", field.JSONName, b)
		}
		switch {
		case av < bv:
			return -1, nil
		case av > bv:
			return 1, nil
		default:
			return 0, nil
		}
	case FieldDecimal:
		ad, err := decimal.NewFromString(a)
		if err != nil {
			return 0, fmt.Errorf("resourcegen: field %q value %q must be a finite decimal", field.JSONName, a)
		}
		bd, err := decimal.NewFromString(b)
		if err != nil {
			return 0, fmt.Errorf("resourcegen: field %q value %q must be a finite decimal", field.JSONName, b)
		}
		return ad.Cmp(bd), nil
	default:
		av, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("resourcegen: field %q value %q must be an integer", field.JSONName, a)
		}
		bv, err := strconv.ParseInt(b, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("resourcegen: field %q value %q must be an integer", field.JSONName, b)
		}
		switch {
		case av < bv:
			return -1, nil
		case av > bv:
			return 1, nil
		default:
			return 0, nil
		}
	}
}

// constraints is the validate-tag form of the declarative options.
func (f Field) constraints() logical.Constraints {
	return logical.Constraints{
		Min:       f.Min,
		Max:       f.Max,
		MaxLength: f.MaxLength,
		Pattern:   f.Pattern,
		Default:   f.Default,
		Enum:      append([]string(nil), f.EnumValues...),
	}
}

// portablePattern compiles the pattern as Go RE2 and rejects anything the
// form's new RegExp(..., "u") does not share. Escapes are an allowlist
// (\d \D \w \W, the single-character controls \n \r \t \f \v, \0 when it is
// NUL, two-digit \xNN, and identity escapes of syntax characters). \b and \B
// are not on it: JavaScript still finds word edges between the surrogates of
// a non-BMP character. \a, octal, \x{HHHH}, \s, \p, and the rest are different
// atoms or a syntax error once the form uses the u flag. A ] that is the first
// member of a class is a member in RE2 and closes an empty class in JavaScript.
// An unescaped ] outside a class is a literal in RE2 and a syntax error in
// unicode mode; write \]. A quantifier count may not have a leading zero:
// RE2 treats {01} as literal text and JavaScript treats it as {1}. ^ and $
// cannot take a quantifier. A - inside a class is a range only between single
// characters; a class escape on either side is rejected, while a hyphen that
// is first or last stays a literal.
func portablePattern(pattern string) error {
	if _, err := regexp.Compile(pattern); err != nil {
		return err
	}
	return jsUnicodePattern(pattern)
}

func patternNotShared() error {
	return fmt.Errorf("pattern must be valid in both Go RE2 and JavaScript")
}

func jsUnicodePattern(pattern string) error {
	r := []rune(pattern)
	// quantifiable is a stack so a group is one atom even when its last member
	// is an anchor. ^+ is not quantifiable; (?:^)+ is.
	quantifiable := []bool{false}
	top := func() bool { return quantifiable[len(quantifiable)-1] }
	set := func(v bool) { quantifiable[len(quantifiable)-1] = v }
	for i := 0; i < len(r); {
		switch r[i] {
		case '\\':
			next, err := acceptEscape(r, i, false)
			if err != nil {
				return err
			}
			i = next
			set(true)
		case '[':
			next, err := acceptClass(r, i)
			if err != nil {
				return err
			}
			i = next
			set(true)
		case '{':
			if !top() {
				return patternNotShared()
			}
			next, err := acceptQuantifier(r, i)
			if err != nil {
				return err
			}
			i = next
			set(false)
		case '*', '+', '?':
			if !top() {
				return patternNotShared()
			}
			i++
			if i < len(r) && r[i] == '?' {
				i++
			}
			set(false)
		case '}', ']':
			return patternNotShared()
		case '(':
			next, err := acceptGroup(r, i)
			if err != nil {
				return err
			}
			i = next
			quantifiable = append(quantifiable, false)
		case ')':
			if len(quantifiable) == 1 {
				return patternNotShared()
			}
			quantifiable = quantifiable[:len(quantifiable)-1]
			set(true)
			i++
		case '^', '$', '|':
			i++
			set(false)
		default:
			i++
			set(true)
		}
	}
	if len(quantifiable) != 1 {
		return patternNotShared()
	}
	return nil
}

func acceptClass(r []rune, i int) (int, error) {
	i++
	if i < len(r) && r[i] == ':' {
		return 0, patternNotShared()
	}
	atFirst := true
	if i < len(r) && r[i] == '^' {
		i++
	}
	prev := ""
	for i < len(r) {
		if r[i] == ']' && atFirst {
			return 0, patternNotShared()
		}
		if r[i] == ']' {
			return i + 1, nil
		}
		if r[i] == '-' && prev != "" && i+1 < len(r) && r[i+1] != ']' {
			if prev != "single" {
				return 0, patternNotShared()
			}
			i++
			kind, next, err := classMember(r, i)
			if err != nil {
				return 0, err
			}
			if kind != "single" {
				return 0, patternNotShared()
			}
			i = next
			prev = "single"
			atFirst = false
			continue
		}
		if r[i] == '[' && i+1 < len(r) && r[i+1] == ':' {
			return 0, patternNotShared()
		}
		kind, next, err := classMember(r, i)
		if err != nil {
			return 0, err
		}
		i = next
		prev = kind
		atFirst = false
	}
	return 0, patternNotShared()
}

func classMember(r []rune, i int) (string, int, error) {
	if i >= len(r) {
		return "", 0, patternNotShared()
	}
	if r[i] != '\\' {
		return "single", i + 1, nil
	}
	next, err := acceptEscape(r, i, true)
	if err != nil {
		return "", 0, err
	}
	switch r[i+1] {
	case 'd', 'D', 'w', 'W':
		return "set", next, nil
	default:
		return "single", next, nil
	}
}

func acceptEscape(r []rune, i int, inClass bool) (int, error) {
	if i+1 >= len(r) {
		return 0, patternNotShared()
	}
	next := i + 2
	switch r[i+1] {
	case 'd', 'D', 'w', 'W', 'n', 'r', 't', 'f', 'v':
		return next, nil
	case '0':
		if next < len(r) && r[next] >= '0' && r[next] <= '9' {
			return 0, patternNotShared()
		}
		return next, nil
	case 'x':
		if next+1 >= len(r) || !isHex(r[next]) || !isHex(r[next+1]) {
			return 0, patternNotShared()
		}
		return next + 2, nil
	case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|':
		return next, nil
	case '-':
		if inClass {
			return next, nil
		}
		return 0, patternNotShared()
	default:
		return 0, patternNotShared()
	}
}

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func acceptQuantifier(r []rune, i int) (int, error) {
	j, err := readCount(r, i+1)
	if err != nil {
		return 0, err
	}
	if j == i+1 {
		return 0, patternNotShared()
	}
	if j < len(r) && r[j] == ',' {
		j++
		j, err = readCount(r, j)
		if err != nil {
			return 0, err
		}
	}
	if j >= len(r) || r[j] != '}' {
		return 0, patternNotShared()
	}
	j++
	if j < len(r) && r[j] == '+' {
		return 0, patternNotShared()
	}
	if j < len(r) && r[j] == '?' {
		j++
	}
	return j, nil
}

// readCount reads a quantifier bound. A lone 0 is zero. A leading zero on a
// longer bound is rejected: RE2 does not treat {01} as a count.
func readCount(r []rune, j int) (int, error) {
	if j >= len(r) || r[j] < '0' || r[j] > '9' {
		return j, nil
	}
	if r[j] == '0' && j+1 < len(r) && r[j+1] >= '0' && r[j+1] <= '9' {
		return 0, patternNotShared()
	}
	j++
	for j < len(r) && r[j] >= '0' && r[j] <= '9' {
		j++
	}
	return j, nil
}

func acceptGroup(r []rune, i int) (int, error) {
	if i+1 < len(r) && r[i+1] == '?' {
		if i+2 >= len(r) || r[i+2] != ':' {
			return 0, patternNotShared()
		}
		return i + 3, nil
	}
	return i + 1, nil
}

// jsFormPattern is the pattern the form and the admin widget compile with the
// u flag. `.` outside a class becomes `[^\n]`, which is RE2's `.`: one code
// point, every character except newline. JavaScript's `.` also excludes `\r`,
// U+2028, and U+2029, and without u it is one UTF-16 code unit.
func jsFormPattern(pattern string) string {
	r := []rune(pattern)
	var b strings.Builder
	inClass := false
	for i := 0; i < len(r); i++ {
		c := r[i]
		if c == '\\' {
			b.WriteRune(c)
			if i+1 < len(r) {
				i++
				b.WriteRune(r[i])
			}
			continue
		}
		if c == '[' {
			inClass = true
		} else if inClass && c == ']' {
			inClass = false
		}
		if c == '.' && !inClass {
			b.WriteString(`[^\n]`)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// decimalSpelling is types.Decimal's schema pattern. A bound or default that
// shopspring accepts but this pattern rejects (1e-2, +1.5, .5, 1.) would
// initialize a form the request then rejects.
var decimalSpelling = regexp.MustCompile(types.DecimalPattern)

// typeAllowsFilter reports whether an exact-match filter query param can be
// generated for this field's type. Exact-match on decimal/time is fiddly to
// coerce and rarely useful (ranges come later, #260), and text columns are for
// search, not equality; both are excluded.
func (f Field) typeAllowsFilter() bool {
	return logical.AllowsFilter(f.logicalKind(), f.relationKind())
}

// typeAllowsSearch reports whether the field is a text-like column ?search= can LIKE.
func (f Field) typeAllowsSearch() bool {
	return logical.AllowsSearch(f.logicalKind(), f.relationKind())
}

// typeAllowsSort reports whether the field maps to a single orderable column.
// Every scalar and the belongs_to foreign key qualifies; has_many / many_to_many
// (multi-row associations) do not.
func (f Field) typeAllowsSort() bool {
	return logical.AllowsSort(f.logicalKind(), f.relationKind())
}

// typeAllowsAggregate reports whether SUM/AVG/MIN/MAX can be applied to this
// field's column. Only the numeric scalars qualify (issue #272): int, int64,
// uint, decimal. Booleans, text, time, enum and relations are excluded — a SUM
// over them is meaningless or driver-dependent.
func (f Field) typeAllowsAggregate() bool {
	return logical.AllowsAggregate(f.logicalKind(), f.relationKind())
}

// gombitPolicy is the model-first `gombit` tag value for this scalar field, or ""
// when the field is a plain content column (untagged fields default to read+write,
// so no tag is needed). A field with any declared query capability is emitted as
// read+write plus those capabilities — read is required because a query capability
// must be response-visible (resourcepolicy's rule), and write keeps the field
// settable as it was under the CLI grammar. The CLI query modifiers thus become
// declared policy on the model, the single source of truth gombit generate reads.
func (f Field) gombitPolicy() string {
	var caps []string
	if f.Filterable {
		caps = append(caps, "filterable")
	}
	if f.Sortable {
		caps = append(caps, "sortable")
	}
	if f.Searchable {
		caps = append(caps, "searchable")
	}
	if f.Aggregatable {
		caps = append(caps, "aggregatable")
	}
	if len(caps) == 0 {
		return ""
	}
	return "read,write," + strings.Join(caps, ",")
}

func (f Field) gormTag() string {
	var parts []string
	switch f.Type {
	case FieldString, FieldEmail, FieldURL, FieldSlug, FieldIP:
		size := 255
		if f.MaxLength > 0 {
			size = f.MaxLength
		}
		parts = append(parts, "size:"+strconv.Itoa(size))
	case FieldText:
		parts = append(parts, "type:text")
	case FieldEnum:
		parts = append(parts, "size:"+strconv.Itoa(enumColumnSize(f.EnumValues)))
	case FieldDecimal:
		parts = append(parts, fmt.Sprintf("type:decimal(%d,%d)", f.Precision, f.Scale))
	case FieldUUID:
		// char(36) is the portable UUID column: SQLite, PostgreSQL, and MySQL
		// all store the canonical 36-character text form.
		parts = append(parts, "type:char(36)")
	case FieldJSON:
		// Text holds the JSON document on every supported driver. Postgres and
		// MySQL accept a native JSON type; SQLite does not.
		parts = append(parts, "type:text")
	case FieldDate:
		parts = append(parts, "type:date")
	}
	if f.Required && !f.Nullable {
		parts = append(parts, "not null")
	}
	// A parsed GORM default replaces the zero value on create, so an explicit
	// 0 or false never reaches the column. The default is applied only when
	// the request or admin body omits the field.
	if check := sqlCheck(f); check != "" {
		parts = append(parts, "check:"+check)
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

// formatAccepts is Huma's validateFormat. A default is a stored value, so
// it has to pass the same predicate the request does.
func formatAccepts(format, s string) bool {
	return !logical.FormatRejects(format, s)
}

// openAPIFormat is the Huma format for a semantic string. Slug is a pattern,
// not a format. Empty for kinds that do not add one.
func (f Field) openAPIFormat() string {
	switch f.Type {
	case FieldEmail:
		return "email"
	case FieldURL:
		return "uri"
	case FieldIP:
		return "ip"
	default:
		return ""
	}
}

// semanticPattern is the built-in pattern for a kind whose meaning is a
// shape rather than an OpenAPI format. Django's slug alphabet: letters,
// digits, hyphens, and underscores.
func (f Field) semanticPattern() string {
	if f.Type == FieldSlug {
		return `^[-a-zA-Z0-9_]+$`
	}
	return ""
}

// formPattern is the pattern the form enforces. An explicit regex wins; a
// slug with none uses the built-in alphabet.
func (f Field) formPattern() string {
	if f.Pattern != "" {
		return f.Pattern
	}
	return f.semanticPattern()
}

func (f Field) takesMaxLength() bool {
	switch f.Type {
	case FieldString, FieldEmail, FieldURL, FieldSlug, FieldIP:
		return true
	default:
		return false
	}
}

// patternRejectsEmpty reports that the pattern does not match "". A
// pattern that cannot be compiled is treated as rejecting empty; the
// parser has already required a portable pattern before this runs.
func patternRejectsEmpty(pattern string) bool {
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return true
	}
	return !re.MatchString("")
}

// pointerType reports a Go field that accepts JSON null. The form posts null
// only for these. A nullable non-pointer number (int, int64, float) still
// posts 0, which is the value that type stores.
func (f Field) pointerType() bool {
	return strings.HasPrefix(f.GoType, "*")
}

// blankIsNull reports that an empty input must be JSON null. Email, URL,
// slug, and IP reject "", and a user regex does too only when it does not
// match "". A pattern such as ^[a-z]*$ stays a plain string. A required
// field stays a plain string so "" still fails. URL and IP are sortable
// exact values, not search text; email and slug stay searchable.
func (f Field) blankIsNull() bool {
	if f.Nullable && f.Type == FieldUint {
		return true
	}
	if f.Required {
		return false
	}
	switch f.Type {
	case FieldTime, FieldDecimal, FieldDate, FieldUUID, FieldEmail, FieldURL, FieldSlug, FieldIP:
		return true
	case FieldString, FieldText:
		return patternRejectsEmpty(f.Pattern)
	default:
		return false
	}
}

// enumColumnSize sizes the varchar column to hold the longest allowed value,
// with headroom so a later value addition rarely needs a column widen.
func sqlCheck(f Field) string {
	col := f.JSONName
	var parts []string
	if f.Min != "" {
		parts = append(parts, col+" >= "+f.Min)
	}
	if f.Max != "" {
		parts = append(parts, col+" <= "+f.Max)
	}
	return strings.Join(parts, " AND ")
}

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
