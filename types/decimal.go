// Package types holds framework value types that are shared by generated
// models, handler DTOs, and the admin data plane. They exist so a single Go
// type flows through the model, the Huma contract (OpenAPI/TS client), and GORM
// persistence without the DTO drifting from the model (see #222 / #218).
package types

import (
	"fmt"

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
type Decimal struct {
	decimal.Decimal
}

// NewDecimal wraps a shopspring decimal.
func NewDecimal(d decimal.Decimal) Decimal {
	return Decimal{Decimal: d}
}

// NewDecimalFromString parses a decimal string (e.g. "19.99").
func NewDecimalFromString(s string) (Decimal, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Decimal{}, err
	}
	return Decimal{Decimal: d}, nil
}

// Schema implements huma.SchemaProvider so the contract represents a Decimal as
// a JSON string (matching its MarshalJSON), not a reflected struct.
func (Decimal) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type:    huma.TypeString,
		Format:  "decimal",
		Pattern: `^-?[0-9]+(\.[0-9]+)?$`,
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
