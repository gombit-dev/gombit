//go:build integration

package types

import (
	"flag"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var postgresDSN = flag.String("types.postgres-dsn", "", "PostgreSQL DSN for types integration tests")

func TestDatePostgresRoundTrip(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -types.postgres-dsn to run the Postgres date round-trip")
	}
	db, err := gorm.Open(postgres.Open(*postgresDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	type dateRoundTrip struct {
		ID   uint
		Born *Date `gorm:"type:date"`
		Due  Date  `gorm:"type:date;not null"`
	}
	if err := db.Migrator().DropTable(&dateRoundTrip{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&dateRoundTrip{})
	})
	if err := db.AutoMigrate(&dateRoundTrip{}); err != nil {
		t.Fatal(err)
	}
	born, err := ParseDate("2026-03-04")
	if err != nil {
		t.Fatal(err)
	}
	due := NewDate(time.Date(2026, 3, 5, 15, 0, 0, 0, time.FixedZone("X", 3600)))
	stored := dateRoundTrip{Born: &born, Due: due}
	if err := db.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var got dateRoundTrip
	if err := db.First(&got, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Born == nil || got.Born.String() != "2026-03-04" {
		t.Fatalf("born = %#v", got.Born)
	}
	if got.Due.String() != "2026-03-05" {
		t.Fatalf("due = %s", got.Due)
	}
	cleared := dateRoundTrip{Due: due}
	if err := db.Create(&cleared).Error; err != nil {
		t.Fatal(err)
	}
	var empty dateRoundTrip
	if err := db.First(&empty, cleared.ID).Error; err != nil {
		t.Fatal(err)
	}
	if empty.Born != nil {
		t.Fatalf("null born read back as %#v", empty.Born)
	}
	if err := db.Create(&dateRoundTrip{}).Error; err == nil {
		t.Fatal("zero date inserted")
	}
}

// TestDateDriverLocationPostgres: pgx returns a date as UTC midnight whatever
// the session's time zone, so PostgreSQL was not affected by #478; the round
// trips keep it that way.
func TestDateDriverLocationPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -types.postgres-dsn to run the Postgres date round trips")
	}
	for _, c := range []struct{ name, timezone string }{
		{"no timezone", ""},
		{"timezone west of UTC", "America/New_York"},
		{"timezone east of UTC", "Asia/Tokyo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn := *postgresDSN
			if c.timezone != "" {
				dsn = withPostgresTimezone(dsn, c.timezone)
			}
			db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if sqlDB, err := db.DB(); err == nil {
				t.Cleanup(func() { _ = sqlDB.Close() })
			}
			if c.timezone != "" {
				var tz string
				if err := db.Raw("SHOW TimeZone").Scan(&tz).Error; err != nil || tz != c.timezone {
					t.Fatalf("session TimeZone = %q (%v); want %q", tz, err, c.timezone)
				}
			}
			checkDateLocation(t, db, dateLocationDays, "%s::text", func(day string) string { return day })
		})
	}
}

// withPostgresTimezone sets the session time zone on a URL or keyword DSN.
// The zone name stays unescaped: gorm.io/driver/postgres reads it from the
// DSN's text.
func withPostgresTimezone(dsn, zone string) string {
	switch {
	case !strings.Contains(dsn, "://"):
		return dsn + " timezone=" + zone
	case strings.Contains(dsn, "?"):
		return dsn + "&timezone=" + zone
	default:
		return dsn + "?timezone=" + zone
	}
}
