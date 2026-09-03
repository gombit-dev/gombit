//go:build integration

package database

import (
	"context"
	"flag"
	"net/http"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
)

var (
	postgresDSN = flag.String("database.postgres-dsn", "", "PostgreSQL DSN for database integration tests")
	mysqlDSN    = flag.String("database.mysql-dsn", "", "MySQL DSN for database integration tests")
)

func TestOpenPostgresRoundTrip(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -database.postgres-dsn to run Postgres integration tests")
	}

	testOpenRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	}, DriverPostgres)
}

func TestOpenMySQLRoundTrip(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -database.mysql-dsn to run MySQL integration tests")
	}

	testOpenRoundTrip(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	}, DriverMySQL)
}

func TestMapPersistErrorPostgresConstraintViolations(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -database.postgres-dsn to run Postgres integration tests")
	}
	db := openIntegrationDB(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	})
	testForeignKeyAndNotNullViolations(t, db)
}

func TestMapPersistErrorMySQLConstraintViolations(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -database.mysql-dsn to run MySQL integration tests")
	}
	db := openIntegrationDB(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	})
	testForeignKeyAndNotNullViolations(t, db)
}

func TestListQueryHelpersPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -database.postgres-dsn to run Postgres integration tests")
	}
	assertListQueryHelpers(t, openIntegrationDB(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	}))
}

func TestListQueryHelpersMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -database.mysql-dsn to run MySQL integration tests")
	}
	assertListQueryHelpers(t, openIntegrationDB(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	}))
}

func TestAggregateHelpersPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -database.postgres-dsn to run Postgres integration tests")
	}
	assertAggregateHelpers(t, openIntegrationDB(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverPostgres,
		DSN:    *postgresDSN,
	}))
}

func TestAggregateHelpersMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -database.mysql-dsn to run MySQL integration tests")
	}
	assertAggregateHelpers(t, openIntegrationDB(t, config.DatabaseConfig{
		Driver: config.DatabaseDriverMySQL,
		DSN:    *mysqlDSN,
	}))
}

type integrationFKCategory struct {
	ID uint `gorm:"primaryKey"`
}

type integrationFKWidget struct {
	ID         uint `gorm:"primaryKey"`
	CategoryID uint
	Category   integrationFKCategory `gorm:"constraint:OnUpdate:CASCADE,OnDelete:RESTRICT;"`
}

type integrationNotNullWidget struct {
	ID   uint    `gorm:"primaryKey"`
	Name *string `gorm:"not null"`
}

func openIntegrationDB(t *testing.T, cfg config.DatabaseConfig) *DB {
	t.Helper()
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&integrationFKWidget{}, &integrationFKCategory{}, &integrationNotNullWidget{})
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}
	})
	return db
}

// testForeignKeyAndNotNullViolations proves, against a real driver, that a
// foreign-key violation and a NOT NULL violation are both classified as a
// D10 validation error rather than internal. The driver error text differs
// across SQLite/Postgres/MySQL (see IsForeignKeyViolation/IsNotNullViolation
// doc comments), so this must run against the real database, not a
// fabricated error string.
func testForeignKeyAndNotNullViolations(t *testing.T, db *DB) {
	t.Helper()
	if err := db.AutoMigrate(&integrationFKCategory{}, &integrationFKWidget{}, &integrationNotNullWidget{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}

	fkErr := db.Create(&integrationFKWidget{CategoryID: 999}).Error
	if fkErr == nil {
		t.Fatal("Create() with a nonexistent foreign key error = nil, want a foreign key violation")
	}
	if !IsForeignKeyViolation(fkErr) {
		t.Fatalf("IsForeignKeyViolation(%v) = false, want true", fkErr)
	}
	mapped := MapPersistError(context.Background(), fkErr, "resource already exists", "persist widget")
	assertMapped(t, mapped, fkErr, http.StatusUnprocessableEntity, contract.CodeValidationError)

	nnErr := db.Create(&integrationNotNullWidget{Name: nil}).Error
	if nnErr == nil {
		t.Fatal("Create() with a nil required field error = nil, want a NOT NULL violation")
	}
	if !IsNotNullViolation(nnErr) {
		t.Fatalf("IsNotNullViolation(%v) = false, want true", nnErr)
	}
	mapped = MapPersistError(context.Background(), nnErr, "resource already exists", "persist widget")
	assertMapped(t, mapped, nnErr, http.StatusUnprocessableEntity, contract.CodeValidationError)

	// A foreign-key violation on delete means something else still
	// references the row being deleted — a state conflict, not the
	// "referenced something invalid" meaning it has on create/update, so
	// this must use MapDeleteError and land as a conflict, not validation.
	category := integrationFKCategory{}
	if err := db.Create(&category).Error; err != nil {
		t.Fatalf("create category fixture: %v", err)
	}
	if err := db.Create(&integrationFKWidget{CategoryID: category.ID}).Error; err != nil {
		t.Fatalf("create widget fixture: %v", err)
	}
	delErr := db.Delete(&category).Error
	if delErr == nil {
		t.Fatal("Delete() of a still-referenced category error = nil, want a foreign key violation")
	}
	if !IsForeignKeyViolation(delErr) {
		t.Fatalf("IsForeignKeyViolation(%v) = false, want true", delErr)
	}
	mapped = MapDeleteError(context.Background(), delErr, "resource is still referenced by other records", "delete widget")
	assertMapped(t, mapped, delErr, http.StatusConflict, "conflict")
}

func testOpenRoundTrip(t *testing.T, cfg config.DatabaseConfig, wantDriver Driver) {
	t.Helper()

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&testWidget{})
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}
	})

	if got := db.Driver(); got != wantDriver {
		t.Fatalf("Driver() = %q, want %q", got, wantDriver)
	}
	if err := db.AutoMigrate(&testWidget{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v, want nil", err)
	}
	if err := db.Create(&testWidget{Name: string(wantDriver)}).Error; err != nil {
		t.Fatalf("Create() error = %v, want nil", err)
	}

	var count int64
	if err := db.Model(&testWidget{}).Where("name = ?", string(wantDriver)).Count(&count).Error; err != nil {
		t.Fatalf("Count() error = %v, want nil", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}
