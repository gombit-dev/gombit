//go:build integration

package types

import (
	"flag"
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
