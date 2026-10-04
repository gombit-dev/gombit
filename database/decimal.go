package database

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// SQLiteDecimalDigits is the most significant digits a decimal value may carry
// when it is written to SQLite (issue #440).
//
// SQLite has no fixed-point decimal type. A decimal(p,s) column has NUMERIC
// affinity, so the text a decimal is written as is converted to INTEGER or
// REAL, and REAL is a float64: 99999999999999.9999 is stored as
// 100000000000000. Fifteen significant digits always survive that round trip,
// so Gombit's SQLite contract is "at most 15 significant digits": a value with
// more is rejected with a 422 instead of being silently changed. The limit is
// deliberately conservative, not a per-value representability check, so it is
// predictable: some longer values (a 16-digit integer) would happen to survive,
// and they are rejected too. PostgreSQL and MySQL store the declared
// decimal(p,s) exactly and have no such limit.
const SQLiteDecimalDigits = 15

var (
	shopspringDecimal = reflect.TypeOf(decimal.Decimal{})
	// decimalColumnType reads precision and scale from a decimal column's
	// declared type, e.g. `gorm:"type:decimal(19,4)"`. GORM keeps that tag as
	// the field's DataType string rather than parsing it.
	decimalColumnType = regexp.MustCompile(`(?i)^\s*(?:decimal|numeric)\s*\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\)`)
)

// registerDecimalCallback rejects, before the SQL runs on every create and
// update, a decimal value the column would not store exactly (issue #440): one
// that does not fit its declared decimal(p,s), which PostgreSQL and MySQL round
// or refuse and SQLite silently changes, and on SQLite one with more than
// SQLiteDecimalDigits significant digits. The failure is a *ValidationError, so
// MapPersistError answers it with a D10 422 on the API and admin write paths
// alike, the same single chokepoint the Validate hook uses.
func registerDecimalCallback(db *gorm.DB, sqlite bool) error {
	check := func(tx *gorm.DB) { runDecimalCheck(tx, sqlite) }
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:decimal", check); err != nil {
		return fmt.Errorf("database: register create decimal callback: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:decimal", check); err != nil {
		return fmt.Errorf("database: register update decimal callback: %w", err)
	}
	return nil
}

func runDecimalCheck(db *gorm.DB, sqlite bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	stmt := db.Statement
	ctx := stmt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	fields := map[string][]string{}
	add := func(f *schema.Field, msg string) {
		key := fieldKey(f)
		for _, have := range fields[key] {
			if have == msg {
				return
			}
		}
		fields[key] = append(fields[key], msg)
	}

	checkStruct := func(rv reflect.Value) {
		for rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return
			}
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct || rv.Type() != stmt.Schema.ModelType {
			return
		}
		for _, f := range stmt.Schema.Fields {
			if f.DBName == "" || !isDecimalType(f.FieldType) {
				continue
			}
			if msg := decimalProblem(f.ReflectValueOf(ctx, rv), f, sqlite); msg != "" {
				add(f, msg)
			}
		}
	}

	rv := stmt.ReflectValue
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			checkStruct(rv.Index(i))
		}
	default:
		checkStruct(rv)
	}
	// Updates(map) and Update(column, value) carry the written values in Dest,
	// keyed by column or field name; Updates(struct) carries a struct there.
	switch dest := stmt.Dest.(type) {
	case map[string]any:
		for key, value := range dest {
			f := stmt.Schema.LookUpField(key)
			if f == nil || !isDecimalType(f.FieldType) {
				continue
			}
			if msg := decimalProblem(reflect.ValueOf(value), f, sqlite); msg != "" {
				add(f, msg)
			}
		}
	default:
		// A struct Dest that is not the model value itself (checked above).
		if dv := reflect.ValueOf(stmt.Dest); dv.IsValid() {
			checkStruct(dv)
		}
	}

	if len(fields) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", fields))
	}
}

// isDecimalType reports whether t (through pointers) is shopspring's
// decimal.Decimal or a struct embedding it, such as types.Decimal.
func isDecimalType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == shopspringDecimal {
		return true
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	f, ok := t.FieldByName("Decimal")
	return ok && f.Anonymous && f.Type == shopspringDecimal
}

// decimalValue extracts the decimal from v, reporting false for a nil pointer
// or any other value.
func decimalValue(v reflect.Value) (decimal.Decimal, bool) {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return decimal.Decimal{}, false
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return decimal.Decimal{}, false
	}
	if v.Type() == shopspringDecimal {
		d, ok := v.Interface().(decimal.Decimal)
		return d, ok
	}
	if v.Kind() == reflect.Struct {
		if inner := v.FieldByName("Decimal"); inner.IsValid() && inner.Type() == shopspringDecimal {
			d, ok := inner.Interface().(decimal.Decimal)
			return d, ok
		}
	}
	return decimal.Decimal{}, false
}

// decimalProblem describes why v would not be stored exactly in f's column,
// or returns "" when it would.
func decimalProblem(v reflect.Value, f *schema.Field, sqlite bool) string {
	d, ok := decimalValue(v)
	if !ok {
		return ""
	}
	precision, scale := decimalPrecisionScale(f)
	return DecimalStorageProblem(d, precision, scale, sqlite)
}

// DecimalStorageProblem describes why d would not be stored exactly in a
// decimal(precision,scale) column, or returns "" when it would (issue #440).
// precision <= 0 means the column declares none, so only the SQLite limit
// applies. Trailing fractional zeros are not digits that need storing: 1.5000
// fits decimal(5,1).
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
		if significant := len(strings.TrimLeft(whole+frac, "0")); significant > SQLiteDecimalDigits {
			return fmt.Sprintf("has %d significant digits; SQLite stores at most %d exactly", significant, SQLiteDecimalDigits)
		}
	}
	return ""
}

// decimalPrecisionScale reads f's declared precision and scale, from
// `precision:`/`scale:` tags or a decimal(p,s)/numeric(p,s) type. It returns
// 0, 0 when the column declares none.
func decimalPrecisionScale(f *schema.Field) (int, int) {
	if f.Precision > 0 {
		return f.Precision, f.Scale
	}
	m := decimalColumnType.FindStringSubmatch(string(f.DataType))
	if m == nil {
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
