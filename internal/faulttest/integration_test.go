//go:build integration

package faulttest_test

import (
	"flag"
	"testing"

	"github.com/gombit-dev/gombit/database"
)

var (
	postgresDSN = flag.String("faulttest.postgres-dsn", "", "PostgreSQL DSN for faulttest integration tests")
	mysqlDSN    = flag.String("faulttest.mysql-dsn", "", "MySQL DSN for faulttest integration tests")
)

func TestPostgresFaults(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -faulttest.postgres-dsn to run Postgres integration tests")
	}
	testDBFaults(t, database.DriverPostgres, *postgresDSN)
}

// TestMySQLFaults: go-sql-driver declines ExecContext for a statement with
// arguments (driver.ErrSkip) unless it interpolates them, so its statements
// take the prepared path; each must still count once.
func TestMySQLFaults(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -faulttest.mysql-dsn to run MySQL integration tests")
	}
	testDBFaults(t, database.DriverMySQL, *mysqlDSN)
}
