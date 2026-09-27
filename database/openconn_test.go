package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
)

type openConnWidget struct {
	ID   uint
	Name string
}

func (w *openConnWidget) Validate(context.Context, *gorm.DB) error {
	if w.Name == "" {
		return errors.New("name is required")
	}
	return nil
}

// TestOpenConnBehavesLikeOpen: a DB over a caller-built handle gets Open's
// setup (model validation on writes, translated errors) and closes the
// handle.
func TestOpenConnBehavesLikeOpen(t *testing.T) {
	conn, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "conn.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenConn(DriverSQLite, conn)
	if err != nil {
		t.Fatal(err)
	}
	if db.Driver() != DriverSQLite || !db.Capabilities().Transactions {
		t.Fatalf("driver %q, capabilities %+v", db.Driver(), db.Capabilities())
	}
	if sqlDB, err := db.SQLDB(); err != nil || sqlDB != conn {
		t.Fatalf("SQLDB = %p, %v; want the handle given", sqlDB, err)
	}
	if err := db.AutoMigrate(&openConnWidget{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&openConnWidget{}).Error; err == nil {
		t.Fatal("an invalid model was written: the validation callback is missing")
	}
	w := openConnWidget{ID: 1, Name: "a"}
	if err := db.Create(&w).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&openConnWidget{ID: 1, Name: "b"}).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("duplicate key = %v, want gorm.ErrDuplicatedKey (TranslateError)", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Ping(); err == nil {
		t.Fatal("Close left the handle open")
	}
}

func TestOpenConnRejectsBadInput(t *testing.T) {
	if _, err := OpenConn(DriverSQLite, nil); err == nil {
		t.Fatal("OpenConn(nil) accepted")
	}
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := OpenConn("oracle", conn); err == nil {
		t.Fatal("OpenConn accepted an unsupported driver")
	}
}

func TestConfigurePoolGivesAHandleOpensSettings(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ConfigurePool(conn, config.DatabaseConfig{Driver: config.DatabaseDriverSQLite})
	if got := conn.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("SQLite MaxOpenConnections = %d, want Open's 1", got)
	}
	ConfigurePool(conn, config.DatabaseConfig{Driver: config.DatabaseDriverPostgres, MaxOpenConns: 7})
	if got := conn.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want the explicit 7", got)
	}
}
