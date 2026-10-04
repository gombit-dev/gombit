// Package types holds framework value types that are shared by generated
// models, handler DTOs, and the admin data plane. They exist so a single Go
// type flows through the model, the Huma contract (OpenAPI/TS client), and GORM
// persistence without the DTO drifting from the model (see #222 / #218).
package types

import (
	"fmt"
	"math"

	"github.com/danielgtaylor/huma/v2"
	"github.com/shopspring/decimal"
)

// Decimal is a fixed-point decimal for money and other exact numeric values.
//
// It wraps shopspring/decimal.Decimal, so it inherits exact arithmetic, JSON
// (a quoted string — no float rounding), text (un)marshaling, and the database
// sql.Scanner / driver.Valuer used by GORM. On top of that it advertises an
// OpenAPI schema (a string with format "decimal") via huma.SchemaProvider, so
// the generated OpenAPI document and TypeScript client see a string instead of
// an opaque object. Use the gorm tag `type:decimal(p,s)` on the model field to
// pin precision/scale for the migration (gombit make resource emits
// decimal(19,4) by default).
//
// A database opened with database.Open refuses, with a 422-mapped
// database.ValidationError, a value its column would not store exactly: one
// that does not fit decimal(p,s), and on SQLite, which has no fixed-point type,
// one with more than database.SQLiteDecimalDigits digits. Without a
// `type:decimal(p,s)` tag (a `precision:`/`scale:` tag does not reach the
// column for this type) the column is the driver's bare decimal, which on
// MySQL is DECIMAL(10,0) (whole numbers only), so pin decimal(p,s) for money.
type Decimal struct {
	decimal.Decimal
}

// MaxDecimalDigits bounds the digits a decimal value may spell: the digits
// String writes for it, before and after the point ("0.05" is 3). It is far
// beyond any money column (MySQL's widest is decimal(65,30)) and exists
// because a decimal's exponent is otherwise unbounded: "1e1000000000" parses
// in microseconds, but formatting or comparing it materialises every digit and
// holds a core and gigabytes of memory (issue #440 review).
//
// It is the one size rule for decimals: parsing (UnmarshalJSON, UnmarshalText,
// NewDecimalFromString), reading (Scan), and the database write guard all
// judge a value with DecimalShapeOf, so a value one of them accepts the others
// accept too. A PostgreSQL numeric with no declared precision can hold more;
// such a value fails to load.
const MaxDecimalDigits = 1000

// maxDecimalSpelling bounds the length of a decimal's text before it is
// parsed. Parsing a long coefficient is quadratic (a million digits take about
// a second), so a spelling no value within MaxDecimalDigits could have is
// refused on its length alone. A database may pad a value's scale with zeros
// (PostgreSQL writes numeric(1000,1000) values to 1000 places), so up to
// MaxDecimalDigits padding zeros are allowed on top of the digits, plus room
// for a sign, a point, quotes, and an exponent.
const maxDecimalSpelling = 3*MaxDecimalDigits + 32

// DecimalShape is how a decimal is written, as String writes it.
type DecimalShape struct {
	// Whole is the number of digits before the point, 0 for a value below 1.
	Whole int
	// Frac is the number of digits after the point, trailing zeros dropped.
	Frac int
	// Significant runs from the first non-zero digit to the last digit
	// written, trailing integer zeros included (1000 has 4, 0.0012 has 2).
	Significant int
}

// Digits is the number of digits String writes: the leading "0" of a value
// below 1 included.
func (s DecimalShape) Digits() int {
	return max(s.Whole, 1) + s.Frac
}

// DecimalShapeOf measures d without formatting it beyond its bounded
// coefficient, and refuses it when its exponent is beyond ±MaxDecimalDigits or
// it spells more than MaxDecimalDigits digits.
//
// The exponent is bounded on its own, whatever the coefficient: formatting or
// comparing a shopspring decimal rescales it, which computes 10^|exponent|
// before the coefficient matters, so "0e1000000000" costs as much as
// "1e1000000000" (issue #440 review). The coefficient's size is bounded from
// its bit length before it is formatted.
func DecimalShapeOf(d decimal.Decimal) (DecimalShape, error) {
	exponent := int(d.Exponent())
	if exponent > MaxDecimalDigits || exponent < -MaxDecimalDigits {
		return DecimalShape{}, errDecimalTooLarge
	}
	c := d.Coefficient()
	if c.Sign() == 0 {
		return DecimalShape{}, nil
	}
	c.Abs(c)
	var n int // digits of c, trailing zeros removed
	if c.IsUint64() {
		v := c.Uint64()
		for v%10 == 0 {
			v /= 10
			exponent++
		}
		for n = 1; v >= 10; v /= 10 {
			n++
		}
	} else {
		if estimate := int(float64(c.BitLen())*0.30102999566398119521) + 1; estimate > 3*MaxDecimalDigits+1 {
			return DecimalShape{}, errDecimalTooLarge
		}
		digits := c.String()
		n = len(digits)
		for n > 1 && digits[n-1] == '0' {
			n--
		}
		exponent += len(digits) - n
	}
	var shape DecimalShape
	if n+exponent > 0 {
		shape.Whole = n + exponent
	}
	if exponent < 0 {
		shape.Frac = -exponent
		shape.Significant = n
	} else {
		shape.Significant = n + exponent
	}
	if shape.Digits() > MaxDecimalDigits {
		return DecimalShape{}, errDecimalTooLarge
	}
	return shape, nil
}

var errDecimalTooLarge = fmt.Errorf("has more than %d digits", MaxDecimalDigits)

// CheckDecimalSize returns an error when d is beyond the size rule; see
// DecimalShapeOf.
func CheckDecimalSize(d decimal.Decimal) error {
	_, err := DecimalShapeOf(d)
	return err
}

// CheckDecimalSpelling refuses decimal text too long for any value within the
// size rule, before it is parsed (parsing a long coefficient is quadratic).
func CheckDecimalSpelling(s string) error {
	if len(s) > maxDecimalSpelling {
		return errDecimalTooLarge
	}
	return nil
}

// spellingShape measures a plain decimal spelling ([sign] digits [. digits]
// [e [sign] digits]) as DecimalShapeOf measures the value it parses to,
// without parsing it: the read and parse paths already hold the text, and
// measuring the parsed value instead copies and formats its coefficient, which
// a wide value (MySQL pads DECIMAL(65,30) to 30 places) pays for on every row.
// ok is false for any other spelling, which the caller measures the slow way;
// TestDecimalSpellingShapeMatchesValueShape pins the two to agree.
func spellingShape[T ~string | ~[]byte](s T) (shape DecimalShape, ok bool, err error) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	intStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intEnd := i
	fracStart, fracEnd := i, i
	if i < len(s) && s[i] == '.' {
		i++
		fracStart = i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		fracEnd = i
	}
	intLen, fracLen := intEnd-intStart, fracEnd-fracStart
	if intLen == 0 && fracLen == 0 {
		return DecimalShape{}, false, nil
	}
	exp := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		negative := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			negative = s[i] == '-'
			i++
		}
		expStart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			if i-expStart >= 9 { // beyond any bounded exponent: the slow path refuses it
				return DecimalShape{}, false, nil
			}
			exp = exp*10 + int(s[i]-'0')
			i++
		}
		if i == expStart {
			return DecimalShape{}, false, nil
		}
		if negative {
			exp = -exp
		}
	}
	if i != len(s) {
		return DecimalShape{}, false, nil
	}
	// The parsed value's exponent keeps trailing fractional zeros, as
	// shopspring's does, and is bounded first, zero included.
	exponent := exp - fracLen
	if exponent > MaxDecimalDigits || exponent < -MaxDecimalDigits {
		return DecimalShape{}, true, errDecimalTooLarge
	}
	// The coefficient's digits: the integer and fraction digits run
	// together, without leading or trailing zeros.
	first, last := -1, -1
	for k := 0; k < intLen+fracLen; k++ {
		var c byte
		if k < intLen {
			c = s[intStart+k]
		} else {
			c = s[fracStart+k-intLen]
		}
		if c != '0' {
			if first < 0 {
				first = k
			}
			last = k
		}
	}
	if first < 0 {
		return DecimalShape{}, true, nil // zero
	}
	total := intLen + fracLen
	n := last - first + 1
	exponent += total - 1 - last // drop the trailing zeros
	if n+exponent > 0 {
		shape.Whole = n + exponent
	}
	if exponent < 0 {
		shape.Frac = -exponent
		shape.Significant = n
	} else {
		shape.Significant = n + exponent
	}
	if shape.Digits() > MaxDecimalDigits {
		return DecimalShape{}, true, errDecimalTooLarge
	}
	return shape, true, nil
}

// checkSpelledDecimal applies the size rule to a value parsed from spelling:
// from the text when it is a plain spelling, else from the value.
func checkSpelledDecimal[T ~string | ~[]byte](spelling T, parsed decimal.Decimal) error {
	if _, ok, err := spellingShape(spelling); ok {
		return err
	}
	return CheckDecimalSize(parsed)
}

// UnmarshalJSON parses a decimal and refuses one beyond MaxDecimalDigits
// before anything formats it. A JSON null leaves d unchanged, as
// encoding/json's convention (and shopspring's own decoder) has it.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	if len(b) > maxDecimalSpelling {
		return fmt.Errorf("decimal %w", errDecimalTooLarge)
	}
	var inner decimal.Decimal
	if err := inner.UnmarshalJSON(b); err != nil {
		return err
	}
	spelling := b
	if len(spelling) >= 2 && spelling[0] == '"' && spelling[len(spelling)-1] == '"' {
		spelling = spelling[1 : len(spelling)-1]
	}
	if err := checkSpelledDecimal(spelling, inner); err != nil {
		return fmt.Errorf("decimal %w", err)
	}
	d.Decimal = inner
	return nil
}

// UnmarshalText parses a decimal and refuses one beyond MaxDecimalDigits
// before anything formats it.
func (d *Decimal) UnmarshalText(b []byte) error {
	if len(b) > maxDecimalSpelling {
		return fmt.Errorf("decimal %w", errDecimalTooLarge)
	}
	var inner decimal.Decimal
	if err := inner.UnmarshalText(b); err != nil {
		return err
	}
	if err := checkSpelledDecimal(b, inner); err != nil {
		return fmt.Errorf("decimal %w", err)
	}
	d.Decimal = inner
	return nil
}

// Scan reads a decimal from the database and refuses one beyond the size
// rule, so a row the guard would refuse to write (stored before the guard
// existed, or by raw SQL) fails to load with an error instead of hanging the
// first formatting of it. A non-finite float is refused too: SQLite stores the
// literal "1e1000000000" in a decimal column as REAL Inf, and shopspring panics
// on it.
func (d *Decimal) Scan(value any) error {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("decimal: cannot scan non-finite %v", v)
		}
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("decimal: cannot scan non-finite %v", v)
		}
	case string:
		if len(v) > maxDecimalSpelling {
			return fmt.Errorf("decimal %w", errDecimalTooLarge)
		}
	case []byte:
		if len(v) > maxDecimalSpelling {
			return fmt.Errorf("decimal %w", errDecimalTooLarge)
		}
	}
	var inner decimal.Decimal
	if err := inner.Scan(value); err != nil {
		return err
	}
	var err error
	switch v := value.(type) {
	case string:
		err = checkSpelledDecimal(v, inner)
	case []byte:
		err = checkSpelledDecimal(v, inner)
	default:
		err = CheckDecimalSize(inner)
	}
	if err != nil {
		return fmt.Errorf("decimal %w", err)
	}
	d.Decimal = inner
	return nil
}

// NewDecimal wraps a shopspring decimal.
func NewDecimal(d decimal.Decimal) Decimal {
	return Decimal{Decimal: d}
}

// NewDecimalFromString parses a decimal string (e.g. "19.99"), refusing one
// beyond MaxDecimalDigits.
func NewDecimalFromString(s string) (Decimal, error) {
	if err := CheckDecimalSpelling(s); err != nil {
		return Decimal{}, fmt.Errorf("decimal %w", err)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Decimal{}, err
	}
	if err := checkSpelledDecimal(s, d); err != nil {
		return Decimal{}, fmt.Errorf("decimal %w", err)
	}
	return Decimal{Decimal: d}, nil
}

// DecimalPattern is the create-body spelling of a decimal. Huma enforces it on
// the string schema. Bounds and defaults must use the same token so the form,
// the magnitude check, and the request accept one spelling.
const DecimalPattern = `^-?[0-9]+(\.[0-9]+)?$`

// Schema implements huma.SchemaProvider so the contract represents a Decimal as
// a JSON string (matching its MarshalJSON), not a reflected struct.
func (Decimal) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type:    huma.TypeString,
		Format:  "decimal",
		Pattern: DecimalPattern,
		Examples: []any{
			"19.99",
		},
	}
}

// MustDecimal parses s and panics if it is not a decimal. Generated mappers use
// it for a default the generator already validated.
func MustDecimal(s string) Decimal {
	d, err := NewDecimalFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// DecimalWithin reports whether d is inside the optional bounds. Empty min or
// max is unbounded on that side. Bounds are decimal magnitudes: Huma's
// minimum/maximum apply only to number and integer schemas, and Decimal's
// schema is a string.
func DecimalWithin(d Decimal, min, max string) error {
	if min != "" {
		bound, err := decimal.NewFromString(min)
		if err != nil {
			return fmt.Errorf("min %q: %w", min, err)
		}
		if d.LessThan(bound) {
			return fmt.Errorf("must be at least %s", min)
		}
	}
	if max != "" {
		bound, err := decimal.NewFromString(max)
		if err != nil {
			return fmt.Errorf("max %q: %w", max, err)
		}
		if d.GreaterThan(bound) {
			return fmt.Errorf("must be at most %s", max)
		}
	}
	return nil
}

// GormDataType tells GORM the default column family when a model field omits an
// explicit `type:` tag. Generated models pin exact precision with
// `gorm:"type:decimal(19,4)"`, which takes precedence over this default.
func (Decimal) GormDataType() string {
	return "decimal"
}
