//go:build integration

package faulttest_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

func TestPostgresLockWait(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -faulttest.postgres-dsn to run Postgres integration tests")
	}
	testLockWait(t, database.DriverPostgres, "pgx", *postgresDSN)
}

func TestMySQLLockWait(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -faulttest.mysql-dsn to run MySQL integration tests")
	}
	testLockWait(t, database.DriverMySQL, "mysql", *mysqlDSN)
}

// testLockWait: AwaitLockWait reports nothing while a row is merely locked,
// and returns once another session's statement on it is blocked behind that
// lock. The blocked statement is an UPDATE, which MySQL reports as "updating",
// and a SELECT ... FOR UPDATE by primary key, which it reports as
// "statistics": the lock auth.lockUserTx takes, and the case AwaitLockWait
// exists for.
//
// The negative check runs once per database, not per waiter, because it has
// to sit out its whole window. On MySQL that window is just over a second:
// the probe only counts a statement whose TIME has reached 1, which takes a
// whole-second boundary, and a window just over a second always holds one,
// so a shorter window could pass whatever the probe matched.
func testLockWait(t *testing.T, kind database.Driver, driver, dsn string) {
	t.Helper()
	const table = "faulttest_lockwait"
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+" (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table) })
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	// lock begins a transaction that holds the row's lock until it ends.
	lock := func(t *testing.T) *sql.Tx {
		t.Helper()
		holder, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = holder.Rollback() })
		if _, err := holder.ExecContext(ctx, "SELECT id FROM "+table+" WHERE id = 1 FOR UPDATE"); err != nil {
			t.Fatal(err)
		}
		return holder
	}

	t.Run("a locked row with no waiter", func(t *testing.T) {
		lock(t)
		window := 200 * time.Millisecond
		if kind == database.DriverMySQL {
			window = 1200 * time.Millisecond
		}
		if err := faulttest.AwaitLockWaitErr(kind, dsn, table, window); err == nil {
			t.Fatal("AwaitLockWait reported a waiter while the row was only locked")
		}
	})

	waiters := []struct{ name, statement string }{
		{"update", "UPDATE " + table + " SET id = 1 WHERE id = 1"},
		{"select for update by primary key", "SELECT id FROM " + table + " WHERE id = 1 FOR UPDATE"},
	}
	for _, w := range waiters {
		t.Run(w.name, func(t *testing.T) {
			holder := lock(t)
			waiter := make(chan error, 1)
			go func() {
				// A transaction, so a SELECT ... FOR UPDATE keeps its lock
				// until the test says; the statement waits either way.
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					waiter <- err
					return
				}
				defer func() { _ = tx.Rollback() }()
				_, err = tx.ExecContext(ctx, w.statement)
				waiter <- err
			}()
			faulttest.AwaitLockWait(t, kind, dsn, table)
			select {
			case err := <-waiter:
				t.Fatalf("the waiter finished (%v) while the row was still locked", err)
			default:
			}
			if err := holder.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waiter:
				if err != nil {
					t.Fatalf("waiter: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the waiter is still blocked after the lock was released")
			}
		})
	}
}
