package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/gombit-dev/gombit/types"
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
	// decimalDDL reads a decimal column type as GORM emits it: precision and
	// scale ("decimal(19,4)", a bare "numeric") and any modifiers after them
	// ("decimal(19,4) unsigned").
	decimalDDL = regexp.MustCompile(`(?i)^\s*(?:decimal|numeric|dec|fixed)\s*(?:\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\))?(\s+.*)?$`)
	// textDDL reads a text column type and its length, if it has one.
	textDDL     = regexp.MustCompile(`(?i)^\s*(?:var)?(?:char|text|clob|string|tinytext|mediumtext|longtext|nvarchar|nchar)\b(?:\s*\(\s*(\d+)\s*\))?`)
	unsignedDDL = regexp.MustCompile(`(?i)\bunsigned\b`)
)

// decimalColumn is what a decimal field's column can hold, read from the DDL
// GORM emits for it on the open dialect.
type decimalColumn struct {
	precision, scale int    // 0, 0: no fixed precision (PostgreSQL numeric, SQLite)
	unsigned         bool   // MySQL "unsigned": no negative values
	text             bool   // a text column stores the decimal's digits as written
	textLength       int    // the text column's length, 0 when unbounded
	unsupported      string // a column type gombit cannot check, e.g. "real"
}

// decimalGuard is the gombit:decimal GORM plugin. Its caches live as long as
// the *gorm.DB it is registered on, not the process: a schema holds its DB's
// whole schema cache, so a process-global map keyed by schemas would pin every
// opened-and-closed DB's schemas for good (per-tenant pools, test suites).
type decimalGuard struct {
	driver  Driver
	fields  sync.Map // *schema.Schema -> map[string]*schema.Field (by DBName)
	columns sync.Map // *schema.Field -> decimalColumn
}

// Name names the plugin.
func (*decimalGuard) Name() string { return "gombit:decimal" }

// Initialize registers the check before every create and update.
func (g *decimalGuard) Initialize(db *gorm.DB) error {
	create := db.Callback().Create().Before("gorm:create")
	if err := create.Register("gombit:decimal", func(tx *gorm.DB) { g.check(tx, true) }); err != nil {
		return fmt.Errorf("database: register create decimal callback: %w", err)
	}
	update := db.Callback().Update().Before("gorm:update")
	if err := update.Register("gombit:decimal", func(tx *gorm.DB) { g.check(tx, false) }); err != nil {
		return fmt.Errorf("database: register update decimal callback: %w", err)
	}
	return nil
}

// registerDecimalCallback rejects, before the SQL runs on every create and
// update, a value assigned to a decimal column that the column would not store
// exactly (issue #440): one that does not fit the column's decimal(p,s), which
// PostgreSQL and MySQL round and SQLite silently changes, one with more than
// SQLiteDecimalDigits digits on SQLite, one beyond types.MaxDecimalDigits on any
// driver, and one that is not a decimal number at all. The failure is a
// *ValidationError, so MapPersistError answers it with a D10 422 on the API and
// admin write paths alike, the same single chokepoint the Validate hook uses.
//
// What a statement writes is forEachAssigned's one definition, shared with the
// other write-path guards: a create's rows (or map) and an upsert's explicit
// ON CONFLICT DO UPDATE values; an update's map or the struct it writes, never
// the model's old values; filtered by Select/Omit. A model without a decimal
// field costs nothing.
func registerDecimalCallback(db *gorm.DB, driver Driver) error {
	return db.Use(&decimalGuard{driver: driver})
}

func (g *decimalGuard) check(db *gorm.DB, creating bool) {
	if db.Error != nil || db.Statement == nil || db.Statement.Schema == nil {
		return
	}
	targets := g.fieldsOf(db.Statement.Schema)
	if len(targets) == 0 {
		return // no decimal column to write: no further work on this write
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
	forEachAssigned(db, creating, targets, func(a assignedValue) {
		d, state := assignedDecimalValue(a.Value)
		switch state {
		case noDecimal:
			return
		case notDecimal:
			report(a.Field, "is not a decimal number")
			return
		}
		col := g.columnOf(db, a.Field)
		if col.unsupported != "" {
			if configErr == nil {
				configErr = fmt.Errorf("database: decimal field %s.%s has column type %q, which cannot be checked for exact storage; declare it decimal(p,s)", db.Statement.Schema.Name, a.Field.Name, col.unsupported)
			}
			return
		}
		if msg := columnProblem(d, col, g.driver == DriverSQLite); msg != "" {
			report(a.Field, msg)
		}
	})

	if configErr != nil {
		_ = db.AddError(configErr)
		return
	}
	if len(problems) > 0 {
		_ = db.AddError(NewValidationError("The request contains invalid fields.", problems))
	}
}

// fieldsOf returns the model's decimal fields by DBName, cached per schema.
func (g *decimalGuard) fieldsOf(s *schema.Schema) map[string]*schema.Field {
	if cached, ok := g.fields.Load(s); ok {
		return cached.(map[string]*schema.Field)
	}
	var fields map[string]*schema.Field
	for _, f := range s.Fields {
		if f.DBName != "" && isDecimalType(f.FieldType) {
			if fields == nil {
				fields = map[string]*schema.Field{}
			}
			fields[f.DBName] = f
		}
	}
	g.fields.Store(s, fields)
	return fields
}

// columnOf reads what f's column holds from the DDL GORM emits for it on db's
// dialect: the migrator's own GormDBDataType-then-Dialector.DataTypeOf lookup,
// so the check judges the column Atlas and AutoMigrate create, not the struct
// tag (GORM ignores precision:/scale: tags for a custom type such as
// types.Decimal). Cached per field.
func (g *decimalGuard) columnOf(db *gorm.DB, f *schema.Field) decimalColumn {
	if cached, ok := g.columns.Load(f); ok {
		return cached.(decimalColumn)
	}
	col := parseDecimalDDL(columnDDL(db, f), g.driver == DriverMySQL)
	g.columns.Store(f, col)
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

// parseDecimalDDL reads a column type as GORM emits it. A decimal type without
// arguments is MySQL's DECIMAL(10,0) there and has no fixed precision
// elsewhere; a text column stores the digits as written, up to its length; any
// other type is reported as unsupported.
func parseDecimalDDL(ddl string, mysql bool) decimalColumn {
	if m := decimalDDL.FindStringSubmatch(ddl); m != nil {
		col := decimalColumn{unsigned: unsignedDDL.MatchString(m[3])}
		switch {
		case m[1] != "":
			col.precision, _ = strconv.Atoi(m[1])
			if m[2] != "" {
				col.scale, _ = strconv.Atoi(m[2])
			}
		case mysql:
			col.precision, col.scale = mysqlDefaultPrecision, mysqlDefaultScale
		}
		return col
	}
	if m := textDDL.FindStringSubmatch(ddl); m != nil {
		col := decimalColumn{text: true}
		if m[1] != "" {
			col.textLength, _ = strconv.Atoi(m[1])
		}
		return col
	}
	return decimalColumn{unsupported: strings.TrimSpace(ddl)}
}

// columnProblem describes why d would not be stored exactly in col, or returns
// "" when it would.
func columnProblem(d decimal.Decimal, col decimalColumn, sqlite bool) string {
	if col.text {
		if err := types.CheckDecimalSize(d); err != nil {
			return err.Error()
		}
		// The column stores the digits as the driver.Valuer writes them; the
		// size bound above makes formatting them cheap.
		if col.textLength > 0 {
			if n := len(d.String()); n > col.textLength {
				return fmt.Sprintf("is %d characters; the column holds %d", n, col.textLength)
			}
		}
		return ""
	}
	if col.unsigned && d.Sign() < 0 {
		return "must not be negative: the column is unsigned"
	}
	return DecimalStorageProblem(d, col.precision, col.scale, sqlite)
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

// assignedDecimalValue reads the decimal a struct field, map value, or clause
// value carries. A field of a decimal type is read through its address, so the
// common case boxes nothing; anything else goes through assignedDecimal.
func assignedDecimalValue(v reflect.Value) (decimal.Decimal, decimalState) {
	if !v.IsValid() {
		return decimal.Decimal{}, noDecimal
	}
	if v.CanAddr() {
		switch p := v.Addr().Interface().(type) {
		case *types.Decimal:
			return p.Decimal, isDecimal
		case *decimal.Decimal:
			return *p, isDecimal
		case **types.Decimal:
			if *p == nil {
				return decimal.Decimal{}, noDecimal
			}
			return (*p).Decimal, isDecimal
		case **decimal.Decimal:
			if *p == nil {
				return decimal.Decimal{}, noDecimal
			}
			return **p, isDecimal
		}
	}
	return assignedDecimal(v.Interface())
}

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
	case types.Decimal:
		return v.Decimal, isDecimal
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

// DecimalStorageProblem describes why d would not be stored exactly in a
// decimal(precision,scale) column, or returns "" when it would (issue #440).
// precision <= 0 means the column has no limit of its own (PostgreSQL's
// unbounded numeric), so only the size bound and the SQLite limit apply.
// Trailing zeros after the point are not digits that need storing: 1.5000 fits
// decimal(5,1).
//
// It never formats d before bounding it. Formatting or comparing a decimal
// rescales it to its exponent, so the exponent is bounded first, zero
// included ("0e1000000000" costs as much as "1e1000000000"), then the
// coefficient's size from its bit length; only then is the (bounded)
// coefficient formatted, once, to count its digits.
func DecimalStorageProblem(d decimal.Decimal, precision, scale int, sqlite bool) string {
	exponent := int(d.Exponent())
	if exponent > types.MaxDecimalDigits || exponent < -types.MaxDecimalDigits {
		return fmt.Sprintf("has more than %d digits", types.MaxDecimalDigits)
	}
	// One copy of the coefficient (shopspring has no allocation-free size
	// accessor; NumDigits computes 10^n for a large one).
	c := d.Coefficient()
	if c.Sign() == 0 {
		return ""
	}
	c.Abs(c)
	var whole, frac, digits, magnitude int
	if c.IsUint64() {
		// The path almost every real value takes: counted, not formatted.
		whole, frac, digits, magnitude = smallCoefficientShape(c.Uint64(), exponent)
	} else {
		if estimate := int(float64(c.BitLen())*0.30102999566398119521) + 1; estimate > types.MaxDecimalDigits+1 {
			return fmt.Sprintf("has more than %d digits", types.MaxDecimalDigits)
		}
		whole, frac, digits, magnitude = coefficientShape(c, exponent)
	}
	if whole+frac > types.MaxDecimalDigits {
		return fmt.Sprintf("has more than %d digits", types.MaxDecimalDigits)
	}
	if precision > 0 {
		if whole > precision-scale {
			return fmt.Sprintf("does not fit decimal(%d,%d): at most %d digits before the decimal point", precision, scale, precision-scale)
		}
		if frac > scale {
			return fmt.Sprintf("does not fit decimal(%d,%d): at most %d digits after the decimal point", precision, scale, scale)
		}
	}
	if sqlite {
		if digits > SQLiteDecimalDigits {
			return fmt.Sprintf("has %d digits; SQLite stores at most %d exactly", digits, SQLiteDecimalDigits)
		}
		if magnitude > sqliteDecimalMaxExponent || magnitude < -sqliteDecimalMaxExponent {
			return fmt.Sprintf("is outside the range SQLite stores exactly (about 1e-%d to 1e%d)", sqliteDecimalMaxExponent, sqliteDecimalMaxExponent)
		}
	}
	return ""
}

// smallCoefficientShape is coefficientShape for a coefficient that fits a
// uint64, computed without formatting it.
func smallCoefficientShape(v uint64, exponent int) (whole, frac, digits, magnitude int) {
	for v%10 == 0 {
		v /= 10
		exponent++
	}
	n := 1
	for x := v; x >= 10; x /= 10 {
		n++
	}
	return shape(n, exponent)
}

// coefficientShape counts the digits of the value |c| × 10^exponent (c
// non-zero and bounded): whole and frac are the digits before and after the
// point as written without trailing fractional zeros, digits runs from the
// first non-zero digit to the last digit written (trailing integer zeros
// included), and magnitude is the decimal exponent of the leading digit.
func coefficientShape(c *big.Int, exponent int) (whole, frac, digits, magnitude int) {
	s := c.String()
	n := len(s)
	for n > 1 && s[n-1] == '0' {
		n--
	}
	return shape(n, exponent+len(s)-n)
}

// shape derives the digit counts from a coefficient of n digits with no
// trailing zero, times 10^exponent.
func shape(n, exponent int) (whole, frac, digits, magnitude int) {
	if n+exponent > 0 {
		whole = n + exponent
	}
	if exponent < 0 {
		frac = -exponent
		digits = n
	} else {
		digits = n + exponent
	}
	return whole, frac, digits, n + exponent - 1
}
