package database

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// SQLiteDecimalDigits is the most digits a decimal value may carry when it is
// written to SQLite (issue #440).
//
// SQLite has no fixed-point decimal type. A decimal(p,s) column has NUMERIC
// affinity, so the text a decimal is written as is converted to INTEGER or
// REAL, and REAL is a float64: 99999999999999.9999 is stored as
// 100000000000000. Within float64's normal range fifteen significant digits
// survive that round trip, so Gombit's SQLite contract is "at most 15 digits",
// counted from the first non-zero digit to the last digit of the value
// (trailing zeros after the point excluded), and a magnitude inside that range
// (sqliteDecimalMaxExponent): a value outside it is rejected with a 422
// instead of being silently changed. The limit is deliberately conservative,
// not a per-value representability check, so it is predictable: some longer
// values (a 16-digit integer) would happen to survive, and they are rejected
// too. PostgreSQL and MySQL store the declared decimal(p,s) exactly and have no
// such limit.
const SQLiteDecimalDigits = 15

// sqliteDecimalMaxExponent bounds the decimal exponent of a value SQLite can
// keep: below about 1e-307 a float64 is subnormal and loses digits (1e-400
// becomes 0), and above about 1e307 it overflows.
const sqliteDecimalMaxExponent = 307

// mysqlDefaultPrecision / mysqlDefaultScale are what MySQL makes of a decimal
// or numeric column declared without arguments: DECIMAL(10,0). An untagged
// types.Decimal (GormDataType "decimal") migrates to it on MySQL.
const (
	mysqlDefaultPrecision = 10
	mysqlDefaultScale     = 0
)

var (
	shopspringDecimal = reflect.TypeOf(decimal.Decimal{})
	nullDecimal       = reflect.TypeOf(decimal.NullDecimal{})
	// decimalDDL reads precision and scale from the type GORM emits for a
	// column ("decimal(19,4)", "numeric", "decimal(19,4) unsigned"), allowing
	// trailing modifiers.
	decimalDDL = regexp.MustCompile(`(?i)^\s*(?:decimal|numeric|dec|fixed)\s*(?:\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\))?(?:\s+.*)?$`)
	textDDL    = regexp.MustCompile(`(?i)^\s*(?:var)?(?:char|text|clob|string|tinytext|mediumtext|longtext|nvarchar|nchar)\b`)

	// decimalFieldCache holds each model schema's decimal fields, so a write
	// to a model without one returns before doing any work.
	decimalFieldCache sync.Map // *schema.Schema -> []*schema.Field
	// decimalColumnCache holds each decimal field's column limits per dialect.
	decimalColumnCache sync.Map // decimalColumnKey -> decimalColumn
)

type decimalColumnKey struct {
	field   *schema.Field
	dialect string
}

// decimalColumn is what a decimal field's column can hold, read from the DDL
// GORM emits for it on the open dialect.
type decimalColumn struct {
	precision, scale int    // 0, 0: no fixed precision (PostgreSQL numeric, SQLite)
	text             bool   // a text column stores the decimal's digits exactly
	unsupported      string // a column type gombit cannot check, e.g. "real"
}

// registerDecimalCallback rejects, before the SQL runs on every create and
// update, a value assigned to a decimal column that the column would not store
// exactly (issue #440): one that does not fit the column's decimal(p,s), which
// PostgreSQL and MySQL round and SQLite silently changes, one with more than
// SQLiteDecimalDigits digits on SQLite, and one that is not a decimal number
// at all. The failure is a *ValidationError, so MapPersistError answers it
// with a D10 422 on the API and admin write paths alike, the same single
// chokepoint the Validate hook uses.
//
// The check is keyed on the assignment set, the columns a statement writes,
// not on the Go values reachable from it. A create checks the rows it inserts
// (or its map) and an upsert's explicit ON CONFLICT DO UPDATE values. An update
// checks only what it assigns: its map, or the fields of the struct it was
// given (any struct type, matched to the model's columns by name), filtered by
// Select/Omit as GORM filters them. The model of an update is checked only when
// it is that struct (Save, Updates(&row)); otherwise it holds the row's old
// values, which the statement is not writing.
func registerDecimalCallback(db *gorm.DB, driver Driver) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:decimal", func(tx *gorm.DB) { runDecimalCheck(tx, driver, true) }); err != nil {
		return fmt.Errorf("database: register create decimal callback: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:decimal", func(tx *gorm.DB) { runDecimalCheck(tx, driver, false) }); err != nil {
		return fmt.Errorf("database: register update decimal callback: %w", err)
	}
	return nil
}

func runDecimalCheck(db *gorm.DB, driver Driver, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	stmt := db.Statement
	fields := decimalFieldsOf(stmt.Schema)
	if len(fields) == 0 {
		return // no decimal column to write: no further work on this write
	}
	ctx := stmt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	selected, restricted := stmt.SelectAndOmitColumns(creating, !creating)
	written := func(column string) bool {
		if v, ok := selected[column]; ok {
			return v
		}
		return !restricted
	}

	var problems map[string][]string
	var configErr error
	report := func(f *schema.Field, msg string) {
		if problems == nil {
			problems = map[string][]string{}
		}
		key := fieldKey(f)
		for _, have := range problems[key] {
			if have == msg {
				return
			}
		}
		problems[key] = append(problems[key], msg)
	}
	check := func(f *schema.Field, value any) {
		d, state := assignedDecimal(value)
		switch state {
		case noDecimal:
			return
		case notDecimal:
			report(f, "is not a decimal number")
			return
		}
		col := decimalColumnOf(db, f)
		if col.unsupported != "" {
			if configErr == nil {
				configErr = fmt.Errorf("database: decimal field %s.%s has column type %q, which cannot be checked for exact storage; declare it decimal(p,s)", stmt.Schema.Name, f.Name, col.unsupported)
			}
			return
		}
		if col.text {
			return // its digits are stored as written
		}
		if msg := DecimalStorageProblem(d, col.precision, col.scale, driver == DriverSQLite); msg != "" {
			report(f, msg)
		}
	}
	// checkStruct checks the decimal columns rv writes: its own decimal fields
	// when it is the model, else the fields of its type that carry a model
	// decimal column's name.
	checkStruct := func(rv reflect.Value) {
		for rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return
			}
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct {
			return
		}
		if rv.Type() == stmt.Schema.ModelType {
			for _, f := range fields {
				if written(f.DBName) {
					check(f, f.ReflectValueOf(ctx, rv).Interface())
				}
			}
			return
		}
		dest := &gorm.Statement{DB: db}
		if !rv.CanAddr() {
			ptr := reflect.New(rv.Type())
			ptr.Elem().Set(rv)
			rv = ptr.Elem()
		}
		if err := dest.Parse(rv.Addr().Interface()); err != nil || dest.Schema == nil {
			return
		}
		for _, f := range fields {
			if !written(f.DBName) {
				continue
			}
			if src := dest.Schema.LookUpField(f.DBName); src != nil {
				check(f, src.ReflectValueOf(ctx, rv).Interface())
			}
		}
	}
	checkMap := func(m map[string]any, filter bool) {
		for key, value := range m {
			f := stmt.Schema.LookUpField(key)
			if f == nil || !isDecimalField(f) || (filter && !written(f.DBName)) {
				continue
			}
			check(f, value)
		}
	}
	var checkDest func(v reflect.Value)
	checkDest = func(v reflect.Value) {
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Map:
			if m, ok := v.Interface().(map[string]any); ok {
				checkMap(m, true)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				checkDest(v.Index(i))
			}
		case reflect.Struct:
			checkStruct(v)
		}
	}

	// The Dest is what the statement writes. A create inserts it: the model
	// rows (struct or slice) or a map. An update assigns it: a map
	// (Updates(map), Update(column, value)) or a struct (Save, Updates(struct)).
	checkDest(reflect.ValueOf(stmt.Dest))
	// An upsert's explicit DO UPDATE values are written too. AssignmentColumns
	// and UpdateAll reuse the inserted (checked) value as a clause.Column.
	if creating {
		if c, ok := stmt.Clauses["ON CONFLICT"]; ok {
			if oc, ok := c.Expression.(clause.OnConflict); ok {
				for _, a := range oc.DoUpdates {
					if f := stmt.Schema.LookUpField(a.Column.Name); f != nil && isDecimalField(f) {
						check(f, a.Value)
					}
				}
			}
		}
	}

	if configErr != nil {
		_ = db.AddError(configErr)
		return
	}
	if len(problems) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", problems))
	}
}

// decimalFieldsOf returns the model's decimal fields, cached per schema.
func decimalFieldsOf(s *schema.Schema) []*schema.Field {
	if cached, ok := decimalFieldCache.Load(s); ok {
		return cached.([]*schema.Field)
	}
	var fields []*schema.Field
	for _, f := range s.Fields {
		if isDecimalField(f) {
			fields = append(fields, f)
		}
	}
	decimalFieldCache.Store(s, fields)
	return fields
}

func isDecimalField(f *schema.Field) bool {
	return f.DBName != "" && isDecimalType(f.FieldType)
}

// isDecimalType reports whether t (through pointers) is shopspring's
// decimal.Decimal or NullDecimal, or a struct embedding decimal.Decimal, such
// as types.Decimal.
func isDecimalType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == shopspringDecimal || t == nullDecimal {
		return true
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	f, ok := t.FieldByName("Decimal")
	return ok && f.Anonymous && f.Type == shopspringDecimal
}

// decimalState is what a value assigned to a decimal column turned out to be.
type decimalState int

const (
	noDecimal  decimalState = iota // nothing to write: nil, an invalid NullDecimal, a SQL expression or column reference
	isDecimal                      // a decimal value
	notDecimal                     // a value that is not a decimal number: refused
)

// assignedDecimal reads the decimal a value assigned to a decimal column
// carries. It is total: a value is a decimal, carries none (nil, an invalid
// NullDecimal, a SQL expression or column reference, which are the database's
// to compute), or is not a decimal number and is refused. It dereferences
// pointers, reads a driver.Valuer's Value (sql.NullString, sql.NullFloat64),
// and coerces by kind, so a *string or *float64 PATCH field and a named type
// (`type Amount string`) are checked like the plain value. A string must be a
// plain decimal ("1,5", "NaN", "1_000" and "" are refused), and a float must
// be finite.
func assignedDecimal(value any) (decimal.Decimal, decimalState) {
	switch v := value.(type) {
	case nil:
		return decimal.Decimal{}, noDecimal
	case clause.Expression, clause.Column, *clause.Column:
		return decimal.Decimal{}, noDecimal
	case decimal.Decimal:
		return v, isDecimal
	case decimal.NullDecimal:
		if !v.Valid {
			return decimal.Decimal{}, noDecimal
		}
		return v.Decimal, isDecimal
	case json.Number:
		return parseDecimal(v.String())
	case []byte:
		return parseDecimal(string(v))
	}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return decimal.Decimal{}, noDecimal
		}
		rv = rv.Elem()
	}
	if rv.Type() == shopspringDecimal || rv.Type() == nullDecimal {
		return assignedDecimal(rv.Interface())
	}
	if rv.Kind() == reflect.Struct {
		if inner := rv.FieldByName("Decimal"); inner.IsValid() && inner.Type() == shopspringDecimal {
			return assignedDecimal(inner.Interface())
		}
	}
	if valuer, ok := rv.Interface().(driver.Valuer); ok {
		dv, err := valuer.Value()
		if err != nil {
			return decimal.Decimal{}, notDecimal
		}
		if dv == nil {
			return decimal.Decimal{}, noDecimal
		}
		if _, again := dv.(driver.Valuer); again {
			return decimal.Decimal{}, notDecimal
		}
		return assignedDecimal(dv)
	}
	switch rv.Kind() {
	case reflect.String:
		return parseDecimal(rv.String())
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return decimal.Decimal{}, notDecimal
		}
		if rv.Kind() == reflect.Float32 {
			return decimal.NewFromFloat32(float32(f)), isDecimal
		}
		return decimal.NewFromFloat(f), isDecimal
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return decimal.NewFromInt(rv.Int()), isDecimal
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return parseDecimal(strconv.FormatUint(rv.Uint(), 10))
	}
	return decimal.Decimal{}, notDecimal
}

func parseDecimal(s string) (decimal.Decimal, decimalState) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Decimal{}, notDecimal
	}
	return d, isDecimal
}

// decimalColumnOf reads what f's column holds from the DDL GORM emits for it on
// db's dialect: the migrator's own GormDBDataType-then-Dialector.DataTypeOf
// lookup, so the check judges the column Atlas and AutoMigrate create, not the
// struct tag (GORM ignores precision:/scale: tags for a custom type such as
// types.Decimal). A decimal type without arguments is MySQL's DECIMAL(10,0)
// there and has no fixed precision elsewhere; a text column stores the digits
// exactly; any other type is reported as unsupported. Cached per field and
// dialect.
func decimalColumnOf(db *gorm.DB, f *schema.Field) decimalColumn {
	key := decimalColumnKey{field: f, dialect: db.Name()}
	if cached, ok := decimalColumnCache.Load(key); ok {
		return cached.(decimalColumn)
	}
	col := parseDecimalDDL(columnDDL(db, f), db.Name() == "mysql")
	decimalColumnCache.Store(key, col)
	return col
}

func columnDDL(db *gorm.DB, f *schema.Field) string {
	if typer, ok := reflect.New(f.IndirectFieldType).Interface().(interface {
		GormDBDataType(*gorm.DB, *schema.Field) string
	}); ok {
		if ddl := typer.GormDBDataType(db, f); ddl != "" {
			return ddl
		}
	}
	return db.DataTypeOf(f)
}

// parseDecimalDDL reads a column type as GORM emits it.
func parseDecimalDDL(ddl string, mysql bool) decimalColumn {
	if m := decimalDDL.FindStringSubmatch(ddl); m != nil {
		if m[1] == "" {
			if mysql {
				return decimalColumn{precision: mysqlDefaultPrecision, scale: mysqlDefaultScale}
			}
			return decimalColumn{}
		}
		precision, _ := strconv.Atoi(m[1])
		scale := 0
		if m[2] != "" {
			scale, _ = strconv.Atoi(m[2])
		}
		return decimalColumn{precision: precision, scale: scale}
	}
	if textDDL.MatchString(ddl) {
		return decimalColumn{text: true}
	}
	return decimalColumn{unsupported: strings.TrimSpace(ddl)}
}

// DecimalStorageProblem describes why d would not be stored exactly in a
// decimal(precision,scale) column, or returns "" when it would (issue #440).
// precision <= 0 means the column has no limit of its own (PostgreSQL's
// unbounded numeric), so only the SQLite limit applies. Trailing zeros after
// the point are not digits that need storing: 1.5000 fits decimal(5,1).
func DecimalStorageProblem(d decimal.Decimal, precision, scale int, sqlite bool) string {
	whole, frac, _ := strings.Cut(d.Abs().String(), ".")
	wholeDigits := len(strings.TrimLeft(whole, "0"))
	if precision > 0 {
		if wholeDigits > precision-scale {
			return fmt.Sprintf("does not fit decimal(%d,%d): at most %d digits before the decimal point", precision, scale, precision-scale)
		}
		if len(frac) > scale {
			return fmt.Sprintf("does not fit decimal(%d,%d): at most %d digits after the decimal point", precision, scale, scale)
		}
	}
	if sqlite && !d.IsZero() {
		if digits := len(strings.TrimLeft(whole+frac, "0")); digits > SQLiteDecimalDigits {
			return fmt.Sprintf("has %d digits; SQLite stores at most %d exactly", digits, SQLiteDecimalDigits)
		}
		// The decimal exponent of the leading digit.
		coefficient := strings.TrimLeft(d.Coefficient().String(), "-")
		if magnitude := int(d.Exponent()) + len(coefficient) - 1; magnitude > sqliteDecimalMaxExponent || magnitude < -sqliteDecimalMaxExponent {
			return fmt.Sprintf("is outside the range SQLite stores exactly (about 1e-%d to 1e%d)", sqliteDecimalMaxExponent, sqliteDecimalMaxExponent)
		}
	}
	return ""
}

// fieldKey names f in a ValidationError the way the API names it: its JSON
// name, else its column.
func fieldKey(f *schema.Field) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.DBName
}
