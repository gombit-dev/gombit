package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a span of time. JSON and text use Go's duration syntax
// ("1h30m", "300ms", "0s", "-1s"), which is what Huma's format "duration"
// accepts. The stored column is a signed bigint of nanoseconds so SQLite,
// PostgreSQL, and MySQL sort the same integer. A zero Duration is "0s" and
// is stored. SQL NULL is a nil *Duration.
type Duration struct {
	d time.Duration
}

// ParseDuration parses a Go duration string.
func ParseDuration(s string) (Duration, error) {
	var d Duration
	if err := d.UnmarshalText([]byte(s)); err != nil {
		return Duration{}, err
	}
	return d, nil
}

// MustDuration parses s and panics if it is not a duration. Generated
// mappers use it for a default the generator already validated.
func MustDuration(s string) Duration {
	d, err := ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Duration returns the span as time.Duration.
func (d Duration) Duration() time.Duration { return d.d }

// String returns the canonical Go duration spelling. "1h30m" becomes "1h30m0s".
func (d Duration) String() string { return d.d.String() }

// MarshalJSON encodes the canonical duration string. Zero is "0s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts null or a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = Duration{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return d.UnmarshalText([]byte(s))
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. An empty string is
// rejected: zero is "0s", and "" is not that span.
func (d *Duration) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return fmt.Errorf("types: duration is empty")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("types: duration %q: %w", s, err)
	}
	d.d = parsed
	return nil
}

// Value implements driver.Valuer. The stored value is nanoseconds.
func (d Duration) Value() (driver.Value, error) {
	return int64(d.d), nil
}

// Scan implements sql.Scanner. Drivers return an integer nanosecond count,
// or the duration text.
func (d *Duration) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Duration{}
		return nil
	case int64:
		d.d = time.Duration(v)
		return nil
	case int:
		d.d = time.Duration(v)
		return nil
	case int32:
		d.d = time.Duration(v)
		return nil
	case string:
		return d.scanText(v)
	case []byte:
		return d.scanText(string(v))
	default:
		return fmt.Errorf("types: cannot scan %T into Duration", src)
	}
}

func (d *Duration) scanText(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*d = Duration{}
		return nil
	}
	if parsed, err := time.ParseDuration(s); err == nil {
		d.d = parsed
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("types: cannot scan duration %q", s)
	}
	d.d = time.Duration(n)
	return nil
}

// GormDataType tells GORM the column family when a model field omits an
// explicit type tag. Generated models also set gorm:"type:bigint".
func (Duration) GormDataType() string { return "bigint" }
