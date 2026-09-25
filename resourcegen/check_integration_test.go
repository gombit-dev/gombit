//go:build integration

package resourcegen

import (
	"flag"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	checkPostgresDSN = flag.String("resourcegen.postgres-dsn", "", "PostgreSQL DSN for check-constraint tests")
	checkMysqlDSN    = flag.String("resourcegen.mysql-dsn", "", "MySQL DSN for check-constraint tests")
)

func TestCheckConstraintRejectsOutOfRangePostgres(t *testing.T) {
	if *checkPostgresDSN == "" {
		t.Skip("set -resourcegen.postgres-dsn")
	}
	db, err := gorm.Open(postgres.Open(*checkPostgresDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckRejects(t, db, "check_people_pg")
}

func TestCheckConstraintRejectsOutOfRangeMySQL(t *testing.T) {
	if *checkMysqlDSN == "" {
		t.Skip("set -resourcegen.mysql-dsn")
	}
	db, err := gorm.Open(mysql.Open(*checkMysqlDSN), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckRejects(t, db, "check_people_my")
}

func assertCheckRejects(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	type person struct {
		ID  uint `gorm:"primaryKey"`
		Age int  `gorm:"check:age >= 0 AND age <= 150"`
	}
	if err := db.Table(table).AutoMigrate(&person{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS " + table) })
	if err := db.Table(table).Create(&person{Age: 200}).Error; err == nil {
		t.Fatal("age 200 must violate the check constraint")
	}
	if err := db.Table(table).Create(&person{Age: 0}).Error; err != nil {
		t.Fatalf("age 0 is inside the range: %v", err)
	}
}
