//go:build integration

package framework

import (
	"flag"
	"net/http"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// The /readyz datastore probe (HOST-2 / ADR-015) runs a driver-agnostic
// PingContext. These tests prove it against the real Postgres and MySQL drivers
// — the SQLite + PostgreSQL + MySQL matrix the working agreement requires — via
// the same flag pattern as ./database, ./auth, and ./admin.
var (
	postgresDSN = flag.String("framework.postgres-dsn", "", "PostgreSQL DSN for framework integration tests")
	mysqlDSN    = flag.String("framework.mysql-dsn", "", "MySQL DSN for framework integration tests")
)

func TestReadyzPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -framework.postgres-dsn to run Postgres readiness integration tests")
	}
	runReadyzDriver(t, config.DatabaseDriverPostgres, *postgresDSN)
}

func TestReadyzMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -framework.mysql-dsn to run MySQL readiness integration tests")
	}
	runReadyzDriver(t, config.DatabaseDriverMySQL, *mysqlDSN)
}

func runReadyzDriver(t *testing.T, driver config.DatabaseDriver, dsn string) {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{Driver: driver, DSN: dsn})
	if err != nil {
		t.Fatalf("database.Open(%s) error = %v", driver, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	app := newTestApp(t, WithDatabase(db))

	// A reachable real datastore is ready.
	rec, body := serveReadyz(t, app)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body.Data.Status != "ready" {
		t.Fatalf("GET /readyz data.status = %q, want %q", body.Data.Status, "ready")
	}

	// Closing the pool makes the real driver's PingContext fail; the app reports
	// not ready, and the driver error never reaches the public probe.
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close() error = %v", err)
	}
	rec, body = serveReadyz(t, app)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz status = %d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if body.Error.Code != "not_ready" || body.Error.Message != reasonDatastore {
		t.Fatalf("GET /readyz error = {%q, %q}, want {not_ready, %q}", body.Error.Code, body.Error.Message, reasonDatastore)
	}
}
