//go:build integration

package auth_test

import (
	"testing"

	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

func init() {
	faultDBs = append(faultDBs,
		faulttest.TestDB{Name: "postgres", Kind: database.DriverPostgres, DSN: func(*testing.T) string { return *postgresDSN }},
		faulttest.TestDB{Name: "mysql", Kind: database.DriverMySQL, DSN: func(*testing.T) string { return *mysqlDSN }},
	)
}
