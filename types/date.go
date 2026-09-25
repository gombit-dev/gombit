package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Date is a calendar date. It is not a timestamp and not a time of day.
//
// JSON and text are "YYYY-MM-DD". GORM's default column type is "date"
// (Postgres DATE, MySQL DATE, SQLite TEXT affinity). A zero Date is the
// string "0001-01-01" in JSON and SQL NULL in the database. JSON null is a
// nil *Date.
type Date struct {
	t time.Time
}

// NewDate returns the calendar date of t in UTC, dropping the clock time.
func NewDate(t time.Time) Date {
	return Date{t: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)}
}

// ParseDate parses "YYYY-MM-DD".
func ParseDate(s string) (Date, error) {
	var d Date
	if err := d.UnmarshalText([]byte(s)); err != nil {
		return Date{}, err
	}
	return d, nil
}

// String returns "YYYY-MM-DD", or "" for the zero date.
func (d Date) String() string {
	if d.t.IsZero() {
		return ""
	}
	return d.t.Format(time.DateOnly)
}

// IsZero reports whether d is the zero date.
func (d Date) IsZero() bool { return d.t.IsZero() }

// Time returns the date as UTC midnight.
func (d Date) Time() time.Time { return d.t }

// MarshalJSON encodes the date as "YYYY-MM-DD", including the zero date.
// JSON null is a nil *Date, not a zero value: a non-pointer field stays a
// string so a non-nullable Huma schema is not contradicted.
func (d Date) MarshalJSON() ([]byte, error) {
	s := d.String()
	if s == "" {
		s = "0001-01-01"
	}
	return json.Marshal(s)
}

// UnmarshalJSON accepts null, "YYYY-MM-DD", or an RFC3339 timestamp (the date
// part is kept). The timestamp form is what admin coercion produces when it
// JSON-encodes a time.Time.
func (d *Date) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = Date{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return d.UnmarshalText([]byte(s))
}

// MarshalText implements encoding.TextMarshaler.
func (d Date) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Date) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" {
		*d = Date{}
		return nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		d.t = t
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		d.t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return nil
	}
	return fmt.Errorf("types: date %q must be YYYY-MM-DD", s)
}

// Value implements driver.Valuer. A zero Date is SQL NULL.
func (d Date) Value() (driver.Value, error) {
	if d.t.IsZero() {
		return nil, nil
	}
	return d.String(), nil
}

// Scan implements sql.Scanner. Drivers return a time.Time, a date string, or
// nil.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case time.Time:
		*d = NewDate(v)
		return nil
	case string:
		return d.UnmarshalText([]byte(v))
	case []byte:
		return d.UnmarshalText(v)
	default:
		return fmt.Errorf("types: cannot scan %T into Date", src)
	}
}

// GormDataType tells GORM the column family when a model field omits an
// explicit type tag. Generated models also set gorm:"type:date".
func (Date) GormDataType() string { return "date" }
