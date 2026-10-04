package types

import (
	"fmt"
	"time"
)

// The range a timestamp may hold to be stored, read back and encoded the same
// way on SQLite, PostgreSQL and MySQL (issue #443). MySQL DATETIME holds years
// 1000..9999 and encoding/json refuses a year outside 0..9999; PostgreSQL
// accepts far more, but a value it stores (year 0 is kept as 1 BC) can then no
// longer be encoded. The bounds keep a day inside years 1000..9999 because a
// value is encoded in whatever Location it is read into (pgx uses time.Local),
// and a zone's offset is under 24h even with the historical local mean time Go
// applies to year 1000 (up to about ±16h, Asia/Manila is -15:56).
var (
	minTime = time.Date(1000, time.January, 2, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(9999, time.December, 30, 23, 59, 59, 0, time.UTC)
)

// The range a calendar date may hold on every supported driver: MySQL DATE
// holds years 1000..9999. A date is not converted between time zones, so it
// needs no margin.
var (
	minDate = NewDate(time.Date(1000, time.January, 1, 0, 0, 0, 0, time.UTC))
	maxDate = NewDate(time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC))
)

// TimeBounds returns the earliest and latest timestamp TimeWithin accepts:
// 1000-01-02T00:00:00Z and 9999-12-30T23:59:59Z.
func TimeBounds() (minimum, maximum time.Time) { return minTime, maxTime }

// DateBounds returns the earliest and latest date DateWithin accepts:
// 1000-01-01 and 9999-12-31.
func DateBounds() (minimum, maximum Date) { return minDate, maxDate }

// TimeWithin reports whether t is a timestamp every supported database can
// store and return (TimeBounds), with a message for the field otherwise.
func TimeWithin(t time.Time) error {
	if t.Before(minTime) || t.After(maxTime) {
		return fmt.Errorf("must be between %s and %s", minTime.Format(time.RFC3339), maxTime.Format(time.RFC3339))
	}
	return nil
}

// DateWithin reports whether d is a date every supported database can store
// (DateBounds), with a message for the field otherwise.
func DateWithin(d Date) error {
	if d.t.Before(minDate.t) || d.t.After(maxDate.t) {
		return fmt.Errorf("must be between %s and %s", minDate, maxDate)
	}
	return nil
}
