package database

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata" // the zones below, wherever the test runs

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/types"
)

type boundRow struct {
	ID   uint `gorm:"primaryKey"`
	Day  types.Date
	When time.Time
}

// testTimeBoundsRoundTrip proves on a real driver that the bounds generated
// create bodies enforce (issue #443, types.TimeBounds and types.DateBounds) are
// storable, come back as stored, and encode as JSON. It reads them with Go's
// time.Local set to far-west and far-east zones, because that is the Location
// a driver (pgx) hands timestamps back in, and so what the day's margin on the
// timestamp bounds is for.
func testTimeBoundsRoundTrip(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&boundRow{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrator().DropTable(&boundRow{}) })
	local := time.Local
	t.Cleanup(func() { time.Local = local })
	zones := []*time.Location{time.UTC}
	for _, name := range []string{"Asia/Manila", "America/Metlakatla", "Pacific/Kiritimati", "Etc/GMT+12"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		zones = append(zones, loc)
	}
	minTime, maxTime := types.TimeBounds()
	minDate, maxDate := types.DateBounds()
	for _, in := range []boundRow{
		{Day: minDate, When: minTime},
		{Day: maxDate, When: maxTime},
	} {
		if err := db.Create(&in).Error; err != nil {
			t.Fatalf("store %s / %s: %v", in.Day, in.When, err)
		}
		for _, zone := range zones {
			time.Local = zone
			var got boundRow
			if err := db.First(&got, in.ID).Error; err != nil {
				t.Fatalf("read %d in zone %s: %v", in.ID, zone, err)
			}
			if got.Day.String() != in.Day.String() {
				t.Errorf("date %s came back as %s (zone %s)", in.Day, got.Day, zone)
			}
			// Drivers round to their precision (MySQL datetime(3), Postgres µs).
			if d := got.When.Sub(in.When); d < -time.Second || d > time.Second {
				t.Errorf("time %s came back as %s (zone %s)", in.When, got.When, zone)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Errorf("bound row read in zone %s does not encode: %v", zone, err)
			}
		}
	}
}

func TestTimeBoundsRoundTripOnSQLite(t *testing.T) {
	db, err := Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "bounds.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	testTimeBoundsRoundTrip(t, db)
}
