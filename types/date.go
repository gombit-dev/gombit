package types

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Date is a calendar date. It is not a timestamp and not a time of day.
//
// JSON and text are "YYYY-MM-DD". GORM's default column type is "date"
// (Postgres DATE, MySQL DATE, SQLite TEXT affinity). Value returns that
// calendar day as UTC midnight; GORM binds GormValue, which is the same
// except on MySQL. Through GORM, a Date keeps its day whatever location the
// driver converts times to (SQLite _loc, MySQL loc, PostgreSQL timezone),
// except, with parseTime=true, a day a MySQL loc skipped whole, which reads
// as the day before (see Scan).
// The zero Date is not a stored value: Value and MarshalJSON reject it.
// JSON null is a nil *Date.
type Date struct {
	t time.Time
}

// NewDate returns the calendar date of t in t's location, dropping the clock
// time.
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

// MarshalJSON encodes the date as "YYYY-MM-DD". The zero date has no JSON
// form: "0001-01-01" is that zero value, and publishing it would round-trip
// through Value as an error. JSON null is a nil *Date.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.t.IsZero() {
		return nil, fmt.Errorf("types: zero date has no JSON form")
	}
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts null, "YYYY-MM-DD", or an RFC3339 timestamp (its date
// at its own offset is kept). The timestamp form is what admin coercion
// produces when it JSON-encodes a time.Time.
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

// UnmarshalText implements encoding.TextUnmarshaler. An RFC3339 timestamp is
// read as its calendar date at the timestamp's own offset:
// "2026-03-04T23:30:00-05:00" is 2026-03-04.
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

// Value implements driver.Valuer. The stored value is UTC midnight on the
// calendar day: the text SQLite has always stored for it
// ("2024-01-15 00:00:00+00:00"), so rows written before still match a
// condition, and the date PostgreSQL stores whatever its time zone. Through
// GORM, GormValue is bound instead. Outside GORM, go-sql-driver/mysql
// converts it to its loc, the evening before west of UTC: pass String()
// there.
// A zero Date is not SQL NULL: NULL is a nil *Date, and the zero value is
// refused so a required column does not silently insert NULL.
func (d Date) Value() (driver.Value, error) {
	if d.t.IsZero() {
		return nil, fmt.Errorf("types: zero date is not a stored value")
	}
	return d.Time(), nil
}

// GormValue implements gorm.Valuer, which GORM binds instead of Value for a
// Date it writes or compares: Create, Save and Updates, and a Where, struct
// or IN condition alike. On MySQL it is the "YYYY-MM-DD" text:
// go-sql-driver/mysql sends a time.Time in its loc, so west of UTC a DATE
// column stored UTC midnight as the day before, and with a loc whose offset
// from UTC on the Date's day is not zero a condition compared the column
// with a DATETIME: an equality matched no row and a range was off by a day
// at its ends (#478).
// Every other dialect gets Value.
func (d Date) GormValue(_ context.Context, db *gorm.DB) clause.Expr {
	if !d.t.IsZero() && db != nil && db.Dialector != nil && db.Name() == "mysql" {
		return clause.Expr{SQL: "?", Vars: []any{d.String()}}
	}
	return clause.Expr{SQL: "?", Vars: []any{dateValue{d}}}
}

// dateValue is a Date bound as a plain driver.Valuer: not being a
// gorm.Valuer, GORM hands it to the driver instead of calling GormValue
// again, and the driver calls Value, which refuses the zero date.
type dateValue struct{ d Date }

func (v dateValue) Value() (driver.Value, error) { return v.d.Value() }

// Scan implements sql.Scanner. Drivers return a time.Time, a date string, or
// nil. A time.Time is the day the driver means, whatever its location:
//   - a UTC midnight is that UTC day. SQLite and PostgreSQL return the UTC
//     midnight Value wrote, and mattn/go-sqlite3 converts it to its _loc
//     (19:00 the day before in America/New_York; #478);
//   - its own day's midnight in its location is that day:
//     go-sql-driver/mysql with parseTime=true returns a DATE as midnight in
//     its loc;
//   - else, the next day's midnight in its location, as time.Date normalizes
//     it, is that next day: where a DST change skips that midnight, time.Date
//     can give 23:00 the day before (America/Sao_Paulo, 2018-11-04);
//   - any other time is its calendar day in its own location, as NewDate
//     reads it.
//
// So Scan reads two kinds of time differently from NewDate (and from Scan
// before #478): a UTC midnight in a location west of UTC is its UTC day, not
// the day before, and a time on the eve that a skipped midnight normalizes
// back to (23:00 in America/Sao_Paulo) is the next day.
//
// With parseTime=true (as the DSN gombit new writes) and a MySQL loc that
// skipped a whole calendar day, such as Pacific/Apia on 2011-12-30,
// go-sql-driver/mysql returns that day as the same time as the day before,
// so it reads as the day before, as it did before #478, and saving a row
// read so stores the day before. It is stored right when written. With
// parseTime=false the driver returns the DATE as text, and that day reads
// back right.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case time.Time:
		*d = dateOfDriverTime(v)
		return nil
	case string:
		return d.UnmarshalText([]byte(v))
	case []byte:
		return d.UnmarshalText(v)
	default:
		return fmt.Errorf("types: cannot scan %T into Date", src)
	}
}

// dateOfDriverTime is the calendar date Scan reads from a driver's t.
func dateOfDriverTime(t time.Time) Date {
	if u := t.UTC(); u.Hour() == 0 && u.Minute() == 0 && u.Second() == 0 && u.Nanosecond() == 0 {
		return NewDate(u)
	}
	day := NewDate(t)
	if time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Equal(t) {
		return day
	}
	next := day.t.AddDate(0, 0, 1)
	if time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, t.Location()).Equal(t) {
		return Date{t: next}
	}
	return day
}

// GormDataType tells GORM the column family when a model field omits an
// explicit type tag. Generated models also set gorm:"type:date".
func (Date) GormDataType() string { return "date" }
