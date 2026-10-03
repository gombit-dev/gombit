//go:build integration

package claims_test

import (
	"flag"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/storage/claims"
)

var (
	postgresDSN = flag.String("claims.postgres-dsn", "", "PostgreSQL DSN for the claims integration tests")
	mysqlDSN    = flag.String("claims.mysql-dsn", "", "MySQL DSN for the claims integration tests")
)

// TestPostgres and TestMySQL run the protocol, the race included, where
// the conditional updates and row locks are the real engines'.
func TestPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -claims.postgres-dsn to run the PostgreSQL claims tests")
	}
	runDriver(t, config.DatabaseDriverPostgres, *postgresDSN)
}

func TestMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -claims.mysql-dsn to run the MySQL claims tests")
	}
	runDriver(t, config.DatabaseDriverMySQL, *mysqlDSN)
}

func runDriver(t *testing.T, driver config.DatabaseDriver, dsn string) {
	db, err := database.Open(config.DatabaseConfig{Driver: driver, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(append(claims.Models(), &doc{})...)
		_ = db.Close()
	})
	runSuite(t, db.DB)
}
