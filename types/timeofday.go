package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TimeOfDay is a clock time with no date and no zone. JSON and text are
// "HH:MM:SS". "HH:MM" and a time with a numeric offset ("15:04:05+07:00",
// the second form Huma's format "time" accepts) are read as that clock and
// stored as "HH:MM:SS". The offset is not applied: a time of day has no date
// to convert.
//
// The zero TimeOfDay is midnight, which is a real clock time, and Value
// stores it. SQL NULL is a nil *TimeOfDay. The column is char(8) so SQLite,
// PostgreSQL, and MySQL share one sortable text form.
type TimeOfDay struct {
	hour, minute, second int
}

// ParseTimeOfDay parses a clock time.
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	var t TimeOfDay
	if err := t.UnmarshalText([]byte(s)); err != nil {
		return TimeOfDay{}, err
	}
	return t, nil
}

// MustTimeOfDay parses s and panics if it is not a clock time. Generated
// mappers use it for a default the generator already validated.
func MustTimeOfDay(s string) TimeOfDay {
	t, err := ParseTimeOfDay(s)
	if err != nil {
		panic(err)
	}
	return t
}

// String returns "HH:MM:SS". Midnight is "00:00:00".
func (t TimeOfDay) String() string {
	return fmt.Sprintf("%02d:%02d:%02d", t.hour, t.minute, t.second)
}

// MarshalJSON encodes the clock as "HH:MM:SS".
func (t TimeOfDay) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

// UnmarshalJSON accepts null, "HH:MM:SS", "HH:MM", or a clock with a numeric offset.
func (t *TimeOfDay) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = TimeOfDay{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return t.UnmarshalText([]byte(s))
}

// MarshalText implements encoding.TextMarshaler.
func (t TimeOfDay) MarshalText() ([]byte, error) {
	return []byte(t.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. An empty string is
// rejected: midnight is "00:00:00", and "" is not that clock.
func (t *TimeOfDay) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return fmt.Errorf("types: time of day is empty")
	}
	if len(s) == len("15:04") {
		s += ":00"
	}
	parsed, err := time.Parse(time.TimeOnly, s)
	if err != nil {
		parsed, err = time.Parse("15:04:05Z07:00", s)
	}
	if err != nil {
		return fmt.Errorf("types: time of day %q must be HH:MM:SS", strings.TrimSpace(string(b)))
	}
	t.hour, t.minute, t.second = parsed.Hour(), parsed.Minute(), parsed.Second()
	return nil
}

// Value implements driver.Valuer. The stored value is the canonical "HH:MM:SS".
func (t TimeOfDay) Value() (driver.Value, error) {
	return t.String(), nil
}

// Scan implements sql.Scanner.
func (t *TimeOfDay) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*t = TimeOfDay{}
		return nil
	case time.Time:
		t.hour, t.minute, t.second = v.Hour(), v.Minute(), v.Second()
		return nil
	case string:
		return t.UnmarshalText([]byte(v))
	case []byte:
		return t.UnmarshalText(v)
	default:
		return fmt.Errorf("types: cannot scan %T into TimeOfDay", src)
	}
}

// GormDataType tells GORM the column family when a model field omits an
// explicit type tag. Generated models also set gorm:"type:char(8)".
func (TimeOfDay) GormDataType() string { return "char(8)" }
