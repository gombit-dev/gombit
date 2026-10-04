package database

import (
	"context"
	"encoding/json"
	"fmt"
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
// 100000000000000. Fifteen significant digits always survive that round trip,
// so Gombit's SQLite contract is "at most 15 digits", counted from the first
// non-zero digit to the last digit of the value (trailing zeros after the
// point excluded): a value with more is rejected with a 422 instead of being
// silently changed. The limit is deliberately conservative, not a per-value
// representability check, so it is predictable: some longer values (a 16-digit
// integer) would happen to survive, and they are rejected too. PostgreSQL and
// MySQL store the declared decimal(p,s) exactly and have no such limit.
const SQLiteDecimalDigits = 15

// mysqlDefaultPrecision / mysqlDefaultScale are what MySQL makes of a decimal
// or numeric column declared without arguments: DECIMAL(10,0). An untagged
// types.Decimal (GormDataType "decimal") migrates to it on MySQL, so a value
// with a fraction is rounded there unless it is checked against those limits.
const (
	mysqlDefaultPrecision = 10
	mysqlDefaultScale     = 0
)

var (
	shopspringDecimal = reflect.TypeOf(decimal.Decimal{})
	nullDecimal       = reflect.TypeOf(decimal.NullDecimal{})
	// decimalColumnType reads the declared type of a decimal column, e.g.
	// `gorm:"type:decimal(19,4)"` or a bare "decimal". GORM keeps that tag as
	// the field's DataType string rather than parsing it.
	decimalColumnType = regexp.MustCompile(`(?i)^\s*(?:decimal|numeric)\s*(?:\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\))?\s*$`)
	// destSchemas caches the parsed schema of an Updates(struct) Dest whose type
	// is not the model's, as GORM's own statement does.
	destSchemas sync.Map
)

// registerDecimalCallback rejects, before the SQL runs on every create and
// update, a value assigned to a decimal column that the column would not store
// exactly (issue #440): one that does not fit the column's decimal(p,s), which
// PostgreSQL and MySQL round and SQLite silently changes, and on SQLite one with
// more than SQLiteDecimalDigits digits. The failure is a *ValidationError, so
// MapPersistError answers it with a D10 422 on the API and admin write paths
// alike, the same single chokepoint the Validate hook uses.
//
// The check is keyed on the assignment set, the columns a statement actually
// writes, not on the Go values reachable from it. A create checks the rows it
// inserts (or its map). An update checks only what it assigns: its map, or the
// fields of the struct it was given, matched to the model's columns by name and
// filtered by Select/Omit as GORM filters them. The model of an update is
// checked only when it is that struct (Save, Updates(&row)); otherwise it holds
// the row's old values, which the statement is not writing.
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

	problems := map[string][]string{}
	check := func(column *schema.Field, value any) {
		if column == nil || column.DBName == "" || !isDecimalType(column.FieldType) || !written(column.DBName) {
			return
		}
		d, ok := assignedDecimal(value)
		if !ok {
			return
		}
		precision, scale := decimalPrecisionScale(column, driver)
		if msg := DecimalStorageProblem(d, precision, scale, driver == DriverSQLite); msg != "" {
			key := fieldKey(column)
			for _, have := range problems[key] {
				if have == msg {
					return
				}
			}
			problems[key] = append(problems[key], msg)
		}
	}
	// checkStruct checks the decimal columns rv (a struct of any type) assigns,
	// matching its fields to the model's columns by name.
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
		src := stmt.Schema
		if rv.Type() != stmt.Schema.ModelType {
			parsed, err := schema.Parse(rv.Addr().Interface(), &destSchemas, db.NamingStrategy)
			if err != nil {
				return
			}
			src = parsed
		}
		for _, f := range src.Fields {
			if f.DBName == "" {
				continue
			}
			check(stmt.Schema.LookUpField(f.DBName), f.ReflectValueOf(ctx, rv).Interface())
		}
	}
	checkMap := func(m map[string]any) {
		for key, value := range m {
			check(stmt.Schema.LookUpField(key), value)
		}
	}
	checkAny := func(v reflect.Value) {
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				return
			}
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Map:
			if m, ok := v.Interface().(map[string]any); ok {
				checkMap(m)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				elem := v.Index(i)
				if elem.Kind() == reflect.Struct {
					if !elem.CanAddr() {
						continue
					}
					elem = elem.Addr()
				}
				checkAnyElem(elem, checkMap, checkStruct)
			}
		case reflect.Struct:
			if v.CanAddr() {
				checkStruct(v.Addr())
			} else {
				ptr := reflect.New(v.Type())
				ptr.Elem().Set(v)
				checkStruct(ptr)
			}
		}
	}

	// The Dest is what the statement writes. A create inserts it: the model
	// rows (struct or slice) or a map. An update assigns it: a map
	// (Updates(map), Update(column, value)) or a struct (Save, Updates(struct)).
	// The update's model is checked only when it is that struct; otherwise it
	// holds the row's old values, which this statement does not write.
	checkAny(reflect.ValueOf(stmt.Dest))

	if len(problems) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", problems))
	}
}

// checkAnyElem dispatches one element of a batch Dest.
func checkAnyElem(v reflect.Value, checkMap func(map[string]any), checkStruct func(reflect.Value)) {
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if m, ok := v.Interface().(map[string]any); ok {
		checkMap(m)
		return
	}
	checkStruct(v)
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

// assignedDecimal reads the decimal a value assigned to a decimal column
// carries. Besides the decimal types themselves it reads the plain values a
// map update or Update(column, value) may carry: a string, a json.Number, a
// float, or an integer, which the driver would otherwise round into the column
// unchecked. A nil, an invalid NullDecimal, and a SQL expression (gorm.Expr,
// clause.Expr) report false: there is no value to check, and an expression is
// the database's to compute.
func assignedDecimal(value any) (decimal.Decimal, bool) {
	switch v := value.(type) {
	case nil:
		return decimal.Decimal{}, false
	case clause.Expr, *clause.Expr, clause.Expression:
		return decimal.Decimal{}, false
	case decimal.Decimal:
		return v, true
	case decimal.NullDecimal:
		return v.Decimal, v.Valid
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(v))
		return d, err == nil
	case json.Number:
		d, err := decimal.NewFromString(v.String())
		return d, err == nil
	case float64:
		return decimal.NewFromFloat(v), true
	case float32:
		return decimal.NewFromFloat32(v), true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		d, err := decimal.NewFromString(fmt.Sprint(v))
		return d, err == nil
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return decimal.Decimal{}, false
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
	return decimal.Decimal{}, false
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
	if sqlite {
		if digits := len(strings.TrimLeft(whole+frac, "0")); digits > SQLiteDecimalDigits {
			return fmt.Sprintf("has %d digits; SQLite stores at most %d exactly", digits, SQLiteDecimalDigits)
		}
	}
	return ""
}

// decimalPrecisionScale reads the precision and scale column f holds on
// driver: from `precision:`/`scale:` tags or a decimal(p,s)/numeric(p,s) type,
// and for a type declared without arguments (or an untagged decimal, whose
// GORM type is "decimal") the driver's own default: DECIMAL(10,0) on MySQL. On
// PostgreSQL such a column is an unbounded numeric and on SQLite it has no
// fixed precision, so it returns 0, 0 (no column limit) there.
func decimalPrecisionScale(f *schema.Field, driver Driver) (int, int) {
	if f.Precision > 0 {
		return f.Precision, f.Scale
	}
	dataType := string(f.DataType)
	if dataType == "" {
		dataType = "decimal"
	}
	m := decimalColumnType.FindStringSubmatch(dataType)
	if m == nil {
		return 0, 0
	}
	if m[1] == "" {
		if driver == DriverMySQL {
			return mysqlDefaultPrecision, mysqlDefaultScale
		}
		return 0, 0
	}
	precision, _ := strconv.Atoi(m[1])
	scale := 0
	if m[2] != "" {
		scale, _ = strconv.Atoi(m[2])
	}
	return precision, scale
}

// fieldKey names f in a ValidationError the way the API names it: its JSON
// name, else its column.
func fieldKey(f *schema.Field) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.DBName
}
