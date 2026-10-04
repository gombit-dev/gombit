// Package types holds framework value types that are shared by generated
// models, handler DTOs, and the admin data plane. They exist so a single Go
// type flows through the model, the Huma contract (OpenAPI/TS client), and GORM
// persistence without the DTO drifting from the model (see #222 / #218).
package types

import (
	"fmt"
	"math/big"

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
// before the point plus those after it, as String would write them. It is far
// beyond any column (PostgreSQL's widest declared numeric, MySQL's
// decimal(65,30)) and exists because a decimal's exponent is otherwise
// unbounded: "1e1000000000" parses in microseconds, but formatting it, as
// String and the driver.Valuer do on every write, materialises every digit and
// holds a core and gigabytes of memory (issue #440 review).
const MaxDecimalDigits = 1000

// CheckDecimalSize returns an error when d's exponent is beyond
// ±MaxDecimalDigits or d would spell more than MaxDecimalDigits digits. It
// never formats d.
//
// The exponent is bounded on its own, whatever the coefficient: formatting or
// comparing a shopspring decimal rescales it, which computes 10^|exponent|
// (and String repeats |exponent| zeros) before the coefficient matters, so
// "0e1000000000" costs as much as "1e1000000000" (issue #440 review). The digit
// count comes from the coefficient's bit length, so a hostile value is refused
// without materialising it.
func CheckDecimalSize(d decimal.Decimal) error {
	if e := d.Exponent(); e > MaxDecimalDigits || e < -MaxDecimalDigits {
		return fmt.Errorf("has more than %d digits", MaxDecimalDigits)
	}
	if digits := decimalSpelledDigits(d); digits > MaxDecimalDigits {
		return fmt.Errorf("has more than %d digits", MaxDecimalDigits)
	}
	return nil
}

// decimalSpelledDigits estimates, without formatting, how many digits d spells
// (an upper bound tight to one digit): the coefficient's digits plus the
// zeros its exponent adds before or after them.
func decimalSpelledDigits(d decimal.Decimal) int {
	c := d.Coefficient()
	if c.Sign() == 0 {
		return 1
	}
	coefficient := coefficientDigits(c)
	exponent := int(d.Exponent())
	if exponent >= 0 {
		return coefficient + exponent
	}
	if -exponent > coefficient {
		return -exponent + 1 // "0." and the fraction
	}
	return coefficient
}

// coefficientDigits is the decimal digit count of |c|. It is estimated from the
// bit length (high by at most one) and formatted only when the estimate is
// small enough for that to be cheap, so a huge coefficient is never formatted.
func coefficientDigits(c *big.Int) int {
	estimate := int(float64(c.BitLen())*0.30102999566398119521) + 1
	if estimate > MaxDecimalDigits+1 {
		return estimate
	}
	return len(new(big.Int).Abs(c).String())
}

// UnmarshalJSON parses a decimal and refuses one beyond MaxDecimalDigits
// before anything formats it. A JSON null leaves d unchanged, as
// encoding/json's convention (and shopspring's own decoder) has it.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var inner decimal.Decimal
	if err := inner.UnmarshalJSON(b); err != nil {
		return err
	}
	if err := CheckDecimalSize(inner); err != nil {
		return fmt.Errorf("decimal %s", err)
	}
	d.Decimal = inner
	return nil
}

// UnmarshalText parses a decimal and refuses one beyond MaxDecimalDigits
// before anything formats it.
func (d *Decimal) UnmarshalText(b []byte) error {
	var inner decimal.Decimal
	if err := inner.UnmarshalText(b); err != nil {
		return err
	}
	if err := CheckDecimalSize(inner); err != nil {
		return fmt.Errorf("decimal %s", err)
	}
	d.Decimal = inner
	return nil
}

// Scan reads a decimal from the database and refuses one beyond
// MaxDecimalDigits, so a row stored before the bound existed (SQLite kept the
// text "1e1000000000" as written) fails to load instead of hanging the first
// formatting of it.
func (d *Decimal) Scan(value any) error {
	var inner decimal.Decimal
	if err := inner.Scan(value); err != nil {
		return err
	}
	if err := CheckDecimalSize(inner); err != nil {
		return fmt.Errorf("decimal %s", err)
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
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Decimal{}, err
	}
	if err := CheckDecimalSize(d); err != nil {
		return Decimal{}, fmt.Errorf("decimal %s", err)
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
