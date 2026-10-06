package faulttest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/database"
)

// lockWaitTimeout bounds AwaitLockWait.
var lockWaitTimeout = 15 * time.Second

// lockWaitPoll is how often AwaitLockWait asks the server.
const lockWaitPoll = 20 * time.Millisecond

// lockWaitQueries count the sessions of the current database whose statement
// mentions table ($1 / ?) and that the server reports waiting for a lock.
//
// PostgreSQL reports it exactly: a backend blocked on a lock has
// wait_event_type 'Lock'.
//
// MySQL's exact views (performance_schema.data_lock_waits, sys.innodb_lock_waits,
// information_schema.INNODB_TRX) need privileges the test user (gombit, which
// has ALL on its own schema only) lacks, and a grant would be setup every
// contributor and CI job repeats. What the same account can read is
// information_schema.PROCESSLIST, which lists its own threads. A statement
// blocked on an InnoDB row lock sits in the state "statistics" (a locking
// SELECT) or "updating" (an UPDATE or DELETE) for as long as it waits
// (COMMAND is "Execute" for a prepared statement, "Query" else). TIME is not
// the elapsed time rounded down: it counts the whole-second boundaries the
// clock has crossed since the statement started, so a statement that started
// at .950 s shows TIME 1 about 50 ms later. TIME >= 1 therefore drops a
// statement that passed through those states without a second boundary in
// between (the statements these tests issue take milliseconds); it is not a
// one-second floor, and a running statement caught there as a second ticks
// over matches too. The probe answers between a few milliseconds and about a
// second later than the exact view would, and it matches by the statement's
// text, which is why callers name the table. The tests order their races with
// their statement hooks (Block, Reached), not with this filter: AwaitLockWait
// adds that the waiter has reached the server, and none of them depends on
// the TIME condition for its outcome.
var lockWaitQueries = map[database.Driver]string{
	database.DriverPostgres: `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database()
		  AND pid <> pg_backend_pid()
		  AND wait_event_type = 'Lock'
		  AND query ILIKE '%' || $1 || '%'`,
	database.DriverMySQL: `SELECT count(*) FROM information_schema.PROCESSLIST
		WHERE ID <> CONNECTION_ID()
		  AND DB = DATABASE()
		  AND COMMAND IN ('Query', 'Execute')
		  AND STATE IN ('statistics', 'updating')
		  AND TIME >= 1
		  AND INFO LIKE CONCAT('%', ?, '%')`,
}

// AwaitLockWait polls the server at dsn until it shows a statement that
// mentions table waiting for a lock another session holds, and fails t if
// none is within 15 seconds. PostgreSQL reports that exactly; on MySQL it is
// the PROCESSLIST filter described at lockWaitQueries, which can also match a
// statement that is only passing through. It is how a test knows a
// concurrent statement has reached the server, instead of sleeping and
// hoping: the statement's own hook (DBFaults.Statement) fires before the
// statement reaches the server, which says nothing about whether it blocked.
// It opens its own connection, so it is not affected by faults or by a pool
// whose connections are all busy. kind must be PostgreSQL or MySQL; SQLite
// has no row locks to wait on.
func AwaitLockWait(t testing.TB, kind database.Driver, dsn, table string) {
	t.Helper()
	if err := awaitLockWait(kind, dsn, table, lockWaitTimeout); err != nil {
		t.Fatal(err)
	}
}

func awaitLockWait(kind database.Driver, dsn, table string, timeout time.Duration) error {
	query, ok := lockWaitQueries[kind]
	if !ok {
		return fmt.Errorf("faulttest: no lock-wait probe for driver %q", kind)
	}
	db, err := sql.Open(sqlDriverNames[kind], dsn)
	if err != nil {
		return fmt.Errorf("faulttest: open lock-wait probe: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var last error
	for {
		var waiting int
		err := db.QueryRowContext(ctx, query, table).Scan(&waiting)
		switch {
		case err == nil && waiting > 0:
			return nil
		case err != nil && ctx.Err() == nil:
			return fmt.Errorf("faulttest: lock-wait probe: %w", err)
		case err != nil:
			last = err
		}
		select {
		case <-ctx.Done():
			if dump := sessionDump(kind, db); dump != "" {
				return fmt.Errorf("faulttest: no statement on %q waited for a lock within %s; sessions:\n%s", table, timeout, dump)
			}
			if last != nil {
				return fmt.Errorf("faulttest: no statement on %q waited for a lock within %s (last probe error: %w)", table, timeout, last)
			}
			return fmt.Errorf("faulttest: no statement on %q waited for a lock within %s", table, timeout)
		case <-time.After(lockWaitPoll):
		}
	}
}

// sessionDump lists the sessions the probe can see, to explain a timeout.
func sessionDump(kind database.Driver, db *sql.DB) string {
	query := `SELECT pid, coalesce(wait_event_type, ''), state, query FROM pg_stat_activity WHERE datname = current_database()`
	if kind == database.DriverMySQL {
		query = `SELECT ID, COMMAND, STATE, CONCAT(TIME, 's ', COALESCE(INFO, '')) FROM information_schema.PROCESSLIST`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()
	var out string
	for rows.Next() {
		var a, b, c, d sql.NullString
		if rows.Scan(&a, &b, &c, &d) == nil {
			out += fmt.Sprintf("  %s | %s | %s | %s\n", a.String, b.String, c.String, d.String)
		}
	}
	return out
}
