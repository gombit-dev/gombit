package admin

import (
	"database/sql"
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/gombit-dev/gombit/field"
	"github.com/gombit-dev/gombit/resourcepolicy"
	"github.com/gombit-dev/gombit/types"
)

// FieldsFrom derives a default []Field from model at registration time.
// It may use reflect on this one type. Do not call it from request handlers.
//
// Name comes from the json tag when present, otherwise the GORM column /
// snake_case of the exported field name. Type is inferred from the Go type.
// Read, write, and hidden follow resourcepolicy — the same gombit tag the
// generated API uses — so a column cannot be secret on the public contract
// and editable here. Primary keys and GORM timestamps stay read-only.
// GORM created_at / updated_at are included when the policy leaves them
// on the response.
func FieldsFrom(model any) ([]Field, error) {
	sch, err := parseSchema(model)
	if err != nil {
		return nil, err
	}
	byCol, err := columnPolicy(sch)
	if err != nil {
		return nil, err
	}
	// UniqueIndex stays empty until indexes are parsed. one_to_one is that
	// single-column unique index, not the raw uniqueIndex tag key.
	sch.ParseIndexes()
	// Foreign-key columns of belongs_to relationships become a relation field
	// (a picker), not a bare integer input (#223).
	belongsToFK := belongsToByFK(sch)
	fields := make([]Field, 0, len(sch.Fields))
	for _, sf := range sch.Fields {
		if sf == nil || !sf.StructField.IsExported() {
			continue
		}
		if sf.DBName == "" {
			continue
		}
		if sf.StructField.Type == reflect.TypeOf(gorm.DeletedAt{}) {
			continue
		}
		name := jsonFieldName(sf)
		if name == "" || name == "-" {
			continue
		}
		pol, ok := byCol[sf.DBName]
		if !ok || sch.FieldsByDBName[sf.DBName] != sf {
			continue
		}
		// Hidden on the public contract (gombit:"-" and soft-delete).
		if !pol.InRequest && !pol.InResponse {
			continue
		}
		readOnly := !pol.InRequest
		required := pol.InRequest && sf.NotNull && !sf.HasDefaultValue && sf.FieldType.Kind() != reflect.Pointer
		writeOnly := pol.InRequest && !pol.InResponse
		serverRequired := serverMustSupply(pol)
		if rel, ok := belongsToFK[sf.DBName]; ok {
			fields = append(fields, Field{
				Name:           name,
				Type:           TypeRelation,
				Required:       required,
				ReadOnly:       readOnly,
				WriteOnly:      writeOnly,
				ServerRequired: serverRequired,
				Column:         sf.DBName,
				Related: &Relation{
					Kind:       relationKindForFK(sf),
					Slug:       rel.FieldSchema.Table,
					LabelField: labelFieldFor(rel.FieldSchema),
				},
			})
			continue
		}
		fields = append(fields, Field{
			Name:           name,
			Type:           inferFieldType(sf),
			Required:       required,
			ReadOnly:       readOnly,
			WriteOnly:      writeOnly,
			ServerRequired: serverRequired,
			Column:         sf.DBName,
		})
	}
	// Many-to-many associations are not in sch.Fields (they have no column), so
	// they would otherwise be dropped from the admin entirely (#221/#223). Emit
	// a relation field for each so it round-trips as a list of related ids. The
	// target slug defaults to the related table name (the gombit slug
	// convention); override it with an explicit admin.Field if it differs.
	for _, rel := range sch.Relationships.Many2Many {
		if rel == nil || rel.Field == nil || rel.FieldSchema == nil {
			continue
		}
		fields = append(fields, Field{
			Name: relationFieldName(rel.Field),
			Type: TypeRelation,
			Related: &Relation{
				Kind:       RelManyToMany,
				Slug:       rel.FieldSchema.Table,
				LabelField: labelFieldFor(rel.FieldSchema),
			},
		})
	}
	// has_many associations are also columnless; emit a read-only relation field
	// so the related children's ids appear (read only) rather than being dropped.
	for _, rel := range sch.Relationships.HasMany {
		if rel == nil || rel.Field == nil || rel.FieldSchema == nil {
			continue
		}
		fields = append(fields, Field{
			Name:     relationFieldName(rel.Field),
			Type:     TypeRelation,
			ReadOnly: true,
			Related: &Relation{
				Kind:       RelHasMany,
				Slug:       rel.FieldSchema.Table,
				LabelField: labelFieldFor(rel.FieldSchema),
			},
		})
	}
	return fields, nil
}

func columnPolicy(sch *schema.Schema) (map[string]resourcepolicy.Resolved, error) {
	in, err := resourcepolicy.FromSchema(sch)
	if err != nil {
		return nil, err
	}
	resolved, err := resourcepolicy.ResolveAll(in)
	if err != nil {
		return nil, err
	}
	out := make(map[string]resourcepolicy.Resolved, len(resolved))
	for _, r := range resolved {
		if r.Column != "" {
			out[r.Column] = r
		}
	}
	return out, nil
}

// serverMustSupply is resourcepolicy's required-column rule for a hook-owned
// field: NOT NULL (or primary key), no default, and nothing the database writes
// on insert. Admin create must fail instead of storing the Go zero value.
func serverMustSupply(r resourcepolicy.Resolved) bool {
	if r.CreateSource != resourcepolicy.CreateSourceServer || r.Column == "" {
		return false
	}
	dbSupplies := r.AutoIncrement || (r.AutoTime && r.Creatable)
	return (r.NotNull || r.PrimaryKey) && !r.HasDefault && !dbSupplies
}

func parseSchema(model any) (*schema.Schema, error) {
	if model == nil {
		return nil, fmt.Errorf("admin: nil model")
	}
	rv := reflect.ValueOf(model)
	if rv.Kind() != reflect.Pointer {
		ptr := reflect.New(rv.Type())
		ptr.Elem().Set(rv)
		model = ptr.Interface()
	} else if rv.IsNil() {
		model = reflect.New(rv.Type().Elem()).Interface()
	}
	sch, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return nil, fmt.Errorf("admin: parse schema: %w", err)
	}
	return sch, nil
}

func jsonFieldName(sf *schema.Field) string {
	tag := sf.StructField.Tag.Get("json")
	if tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		name = strings.TrimSpace(name)
		if name != "" {
			return name
		}
	}
	if sf.DBName != "" {
		return sf.DBName
	}
	return toSnake(sf.Name)
}

func inferFieldType(sf *schema.Field) FieldType {
	return FieldType(field.KindFromGo(sf.FieldType, string(sf.DataType)).AdminWire())
}

// relationKindForFK is one_to_one when the foreign key is a single unique
// column, and belongs_to otherwise. Uniqueness is what GORM computed in
// ParseIndexes: the unique tag, or a single-column unique index. A composite
// uniqueIndex is not a unique column, and the raw UNIQUEINDEX tag key is not
// consulted.
func relationKindForFK(sf *schema.Field) string {
	if sf == nil {
		return RelBelongsTo
	}
	if sf.Unique || sf.UniqueIndex != "" {
		return RelOneToOne
	}
	return RelBelongsTo
}

// belongsToByFK maps each belongs_to foreign-key column (on this schema) to its
// relationship, so FieldsFrom can render the FK as a picker.
func belongsToByFK(sch *schema.Schema) map[string]*schema.Relationship {
	out := map[string]*schema.Relationship{}
	for _, rel := range sch.Relationships.BelongsTo {
		if rel == nil || rel.FieldSchema == nil {
			continue
		}
		for _, ref := range rel.References {
			if ref != nil && ref.ForeignKey != nil && ref.ForeignKey.DBName != "" {
				out[ref.ForeignKey.DBName] = rel
			}
		}
	}
	return out
}

// labelFieldFor picks a human label for a related model — the JSON field name
// of its "name" column when present, otherwise empty (the picker falls back to
// the primary key). It returns the field name, not the SQL column: label_field
// sits next to other field names in meta (ADR-013), and the SPA indexes it as a
// key of the data-plane list row (whose keys are field names, not columns).
func labelFieldFor(sch *schema.Schema) string {
	for _, f := range sch.Fields {
		if f != nil && f.DBName == "name" {
			return jsonFieldName(f)
		}
	}
	return ""
}

// detectVersionField finds an integer column named "version" for optimistic
// locking. Pointer and non-integer "version" columns are ignored.
func detectVersionField(sch *schema.Schema) *versionField {
	for _, sf := range sch.Fields {
		if sf == nil || sf.DBName != "version" {
			continue
		}
		// Only a non-pointer integer column drives optimistic locking. A pointer
		// version column is ignored (reflectVersionInt/Set no-op on pointers, so
		// admitting it would guard on version=0 and never bump).
		switch sf.FieldType.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			return nil
		}
		index := sf.StructField.Index
		return &versionField{
			name:   jsonFieldName(sf),
			column: sf.DBName,
			get:    func(inst any) int64 { return reflectVersionInt(inst, index) },
			set:    func(inst any, v int64) { reflectSetVersionInt(inst, index, v) },
		}
	}
	return nil
}

func reflectVersionInt(inst any, index []int) int64 {
	rv := reflect.ValueOf(inst)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return 0
		}
		rv = rv.Elem()
	}
	f := rv.FieldByIndex(index)
	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return f.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := f.Uint()
		if u > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(u)
	default:
		return 0
	}
}

func reflectSetVersionInt(inst any, index []int, v int64) {
	rv := reflect.ValueOf(inst)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	f := rv.FieldByIndex(index)
	if !f.CanSet() {
		return
	}
	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f.SetInt(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v >= 0 {
			f.SetUint(uint64(v))
		}
	}
}

func derivePK(sch *schema.Schema) (name, column string, ft FieldType, err error) {
	for _, sf := range sch.PrimaryFields {
		if sf == nil {
			continue
		}
		return jsonFieldName(sf), sf.DBName, inferFieldType(sf), nil
	}
	for _, sf := range sch.Fields {
		if sf != nil && sf.PrimaryKey {
			return jsonFieldName(sf), sf.DBName, inferFieldType(sf), nil
		}
	}
	return "", "", "", fmt.Errorf("admin: model has no primary key")
}

func matchSchemaField(sch *schema.Schema, f Field) *schema.Field {
	for _, sf := range sch.Fields {
		if sf == nil {
			continue
		}
		if f.Column != "" && sf.DBName == f.Column {
			return sf
		}
		if jsonFieldName(sf) == f.Name || sf.DBName == f.Name || sf.Name == f.Name {
			return sf
		}
	}
	return nil
}

func implicitTimestampColumns(sch *schema.Schema) map[string]implicitColumn {
	out := map[string]implicitColumn{}
	for _, sf := range sch.Fields {
		if sf == nil {
			continue
		}
		var name string
		switch {
		case sf.AutoCreateTime > 0 || sf.DBName == ImplicitCreatedAt:
			name = ImplicitCreatedAt
		case sf.AutoUpdateTime > 0 || sf.DBName == ImplicitUpdatedAt:
			name = ImplicitUpdatedAt
		default:
			continue
		}
		if sf.DBName == "" {
			continue
		}
		out[name] = implicitColumn{
			column: sf.DBName,
			get:    makeGetter(sf.StructField.Index, TypeDateTime),
		}
	}
	return out
}

func makeGetter(index []int, ft FieldType) func(any) any {
	return func(inst any) any {
		rv := reflect.ValueOf(inst)
		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return nil
			}
			rv = rv.Elem()
		}
		field := rv.FieldByIndex(index)
		if !field.IsValid() {
			return nil
		}
		if field.Kind() == reflect.Pointer {
			if field.IsNil() {
				return nil
			}
			field = field.Elem()
		}
		val := field.Interface()
		if ft == TypeDate {
			switch t := val.(type) {
			case time.Time:
				return t.Format(time.DateOnly)
			case types.Date:
				return t.String()
			}
		}
		return val
	}
}

func makeSetter(index []int, ft FieldType, destType reflect.Type) func(any, any) error {
	return func(inst any, raw any) error {
		coerced, err := coerceValue(raw, ft)
		if err != nil {
			return err
		}
		rv := reflect.ValueOf(inst)
		if rv.Kind() == reflect.Pointer {
			rv = rv.Elem()
		}
		field := rv.FieldByIndex(index)
		if !field.CanSet() {
			return fmt.Errorf("field is not settable")
		}
		// JSON null (coerced == nil) clears the dest: nil pointer, "", zero
		// time, or a nil json.RawMessage/[]byte. Missing PATCH keys never
		// reach this setter, so the update stays partial.
		assigned, err := convertTo(coerced, destType)
		if err != nil {
			return err
		}
		field.Set(assigned)
		return nil
	}
}

func convertTo(val any, dest reflect.Type) (reflect.Value, error) {
	if val == nil {
		return reflect.Zero(dest), nil
	}
	if dest.Kind() == reflect.Pointer {
		inner, err := convertTo(val, dest.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		ptr := reflect.New(dest.Elem())
		ptr.Elem().Set(inner)
		return ptr, nil
	}
	src := reflect.ValueOf(val)
	if !src.IsValid() {
		return reflect.Zero(dest), nil
	}
	if src.Type().AssignableTo(dest) {
		return src, nil
	}
	// A named []byte that unmarshals JSON (types.JSON) is convertible from a
	// string and is a byte slice, so the shortcuts below would store the raw
	// characters and skip UnmarshalJSON. The document rules have to run first.
	if out, err, ok := unmarshalDocument(val, src, dest); ok {
		return out, err
	}
	if src.Type().ConvertibleTo(dest) {
		return src.Convert(dest), nil
	}
	if dest.Kind() == reflect.String {
		return reflect.ValueOf(fmt.Sprint(val)).Convert(dest), nil
	}

	out := reflect.New(dest)
	if tu, ok := out.Interface().(encoding.TextUnmarshaler); ok {
		if text, err := asTextBytes(val); err == nil {
			if err := tu.UnmarshalText(text); err != nil {
				return reflect.Value{}, err
			}
			return out.Elem(), nil
		}
	}

	payload, jsonErr := asJSONBytes(val)
	if dest.Kind() == reflect.Slice && dest.Elem().Kind() == reflect.Uint8 {
		if jsonErr != nil {
			return reflect.Value{}, fmt.Errorf("cannot assign %T to %s", val, dest)
		}
		slice := reflect.MakeSlice(dest, len(payload), len(payload))
		reflect.Copy(slice, reflect.ValueOf(payload))
		return slice, nil
	}
	if ju, ok := out.Interface().(json.Unmarshaler); ok && jsonErr == nil {
		if err := ju.UnmarshalJSON(payload); err != nil {
			return reflect.Value{}, err
		}
		return out.Elem(), nil
	}
	switch dest.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		if jsonErr != nil {
			return reflect.Value{}, fmt.Errorf("cannot assign %T to %s", val, dest)
		}
		if err := json.Unmarshal(payload, out.Interface()); err != nil {
			return reflect.Value{}, fmt.Errorf("cannot assign %T to %s", val, dest)
		}
		return out.Elem(), nil
	}
	if scanner, ok := out.Interface().(sql.Scanner); ok {
		if err := scanner.Scan(val); err != nil {
			return reflect.Value{}, err
		}
		return out.Elem(), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot assign %T to %s", val, dest)
}

// unmarshalDocument applies json.Unmarshaler when the destination would
// otherwise be built by ConvertibleTo or by copying JSON bytes into a
// []byte. Other destinations keep the text and struct paths below.
func unmarshalDocument(val any, src reflect.Value, dest reflect.Type) (reflect.Value, error, bool) {
	byteSlice := dest.Kind() == reflect.Slice && dest.Elem().Kind() == reflect.Uint8
	if !byteSlice && !src.Type().ConvertibleTo(dest) {
		return reflect.Value{}, nil, false
	}
	out := reflect.New(dest)
	ju, ok := out.Interface().(json.Unmarshaler)
	if !ok {
		return reflect.Value{}, nil, false
	}
	payload, err := documentPayload(val, dest)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("cannot assign %T to %s", val, dest), true
	}
	if err := ju.UnmarshalJSON(payload); err != nil {
		return reflect.Value{}, err, true
	}
	return out.Elem(), nil, true
}

// documentPayload is the JSON text UnmarshalJSON should see. types.JSON
// must see the encoding of the Go value: a string whose characters are
// already JSON is still a JSON string, and asJSONBytes would hand those
// characters over as raw text. json.RawMessage keeps that raw-text shortcut.
func documentPayload(val any, dest reflect.Type) ([]byte, error) {
	if dest.PkgPath() == "github.com/gombit-dev/gombit/types" && (dest.Name() == "JSON" || dest.Name() == "NullJSON") {
		return json.Marshal(val)
	}
	return asJSONBytes(val)
}

func asTextBytes(val any) ([]byte, error) {
	switch v := val.(type) {
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	case json.RawMessage:
		return v, nil
	default:
		return nil, fmt.Errorf("not text")
	}
}

func asJSONBytes(val any) ([]byte, error) {
	switch v := val.(type) {
	case json.RawMessage:
		if json.Valid(v) {
			return v, nil
		}
		return json.Marshal(v)
	case []byte:
		if json.Valid(v) {
			return v, nil
		}
		return json.Marshal(v)
	case string:
		b := []byte(v)
		if json.Valid(b) {
			return b, nil
		}
		return json.Marshal(v)
	default:
		return json.Marshal(val)
	}
}

func makeNewInstance(elem reflect.Type) func() any {
	return func() any {
		return reflect.New(elem).Interface()
	}
}

func makeNewSlice(elem reflect.Type) func() any {
	sliceType := reflect.SliceOf(elem)
	return func() any {
		return reflect.New(sliceType).Interface()
	}
}

func makeForEach(elem reflect.Type) func(any, func(any)) {
	return func(slicePtr any, fn func(any)) {
		rv := reflect.ValueOf(slicePtr)
		if rv.Kind() == reflect.Pointer {
			rv = rv.Elem()
		}
		for i := 0; i < rv.Len(); i++ {
			item := rv.Index(i)
			if item.Kind() == reflect.Pointer {
				fn(item.Interface())
				continue
			}
			fn(item.Addr().Interface())
		}
	}
}

func elemTypeOf(model any) (reflect.Type, error) {
	if model == nil {
		return nil, fmt.Errorf("admin: nil model")
	}
	t := reflect.TypeOf(model)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("admin: model must be a struct")
	}
	return t, nil
}

func toSnake(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func titleFromSlug(slug string) string {
	parts := strings.FieldsFunc(slug, func(r rune) bool {
		return r == '-' || r == '_'
	})
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

func validIdent(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func validSlug(slug string) bool {
	if slug == "" {
		return false
	}
	for i, r := range slug {
		if i == 0 {
			if r < 'a' || r > 'z' {
				return false
			}
			continue
		}
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
