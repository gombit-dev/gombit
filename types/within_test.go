package types

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below, wherever the test runs
)

func TestTimeWithin(t *testing.T) {
	for _, tc := range []struct {
		t  time.Time
		ok bool
	}{
		{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{time.Date(999, 12, 31, 0, 0, 0, 0, time.UTC), false},
		{time.Date(1000, 1, 1, 23, 59, 59, 0, time.UTC), false},
		{minTime, true},
		{time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), true},
		{maxTime, true},
		{maxTime.Add(time.Nanosecond), false},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), false},
		// The bound is an instant: an offset that lands inside it is inside.
		{time.Date(1000, 1, 1, 22, 0, 0, 0, time.FixedZone("-03", -3*3600)), true},
	} {
		if err := TimeWithin(tc.t); (err == nil) != tc.ok {
			t.Errorf("TimeWithin(%s) = %v, want ok=%v", tc.t.Format(time.RFC3339Nano), err, tc.ok)
		}
	}
	// Every in-range instant encodes as JSON in whatever Location it is read
	// into: the extreme modern offsets, and the historical local mean time Go
	// applies to year 1000, which goes past ±15h in a few zones.
	zones := []*time.Location{time.FixedZone("", -12*3600), time.FixedZone("", 14*3600)}
	for _, name := range []string{"Asia/Manila", "America/Metlakatla", "America/Juneau", "Pacific/Kiritimati"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		zones = append(zones, loc)
	}
	minimum, maximum := TimeBounds()
	for _, edge := range []time.Time{minimum, maximum} {
		for _, loc := range zones {
			if _, err := edge.In(loc).MarshalJSON(); err != nil {
				t.Errorf("%s in %s does not encode: %v", edge, loc, err)
			}
		}
	}
}

func TestDateWithin(t *testing.T) {
	for _, tc := range []struct {
		d  string
		ok bool
	}{
		{"0000-01-01", false},
		{"0999-12-31", false},
		{"1000-01-01", true},
		{"2026-10-04", true},
		{"9999-12-31", true},
	} {
		d, err := ParseDate(tc.d)
		if err != nil {
			t.Fatal(err)
		}
		if err := DateWithin(d); (err == nil) != tc.ok {
			t.Errorf("DateWithin(%s) = %v, want ok=%v", tc.d, err, tc.ok)
		}
	}
}
