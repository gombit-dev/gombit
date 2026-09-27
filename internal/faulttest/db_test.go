package faulttest_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

type faultParent struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (faultParent) TableName() string { return "faulttest_parents" }

type faultChild struct {
	ID       uint `gorm:"primaryKey"`
	ParentID uint
	Name     string
}

func (faultChild) TableName() string { return "faulttest_children" }

// touches reports whether query writes table.
func touches(table string) func(string) bool {
	return func(query string) bool {
		return strings.Contains(query, "INSERT") && strings.Contains(query, table)
	}
}

// openFaultDB opens kind's database at dsn with faults, over tables made
// fresh through a plain connection, so setup statements never reach them.
func openFaultDB(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults) *database.DB {
	t.Helper()
	plain, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriver(kind), DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = plain.Migrator().DropTable(&faultChild{}, &faultParent{})
		_ = plain.Close()
	})
	if err := plain.Migrator().DropTable(&faultChild{}, &faultParent{}); err != nil {
		t.Fatal(err)
	}
	if err := plain.AutoMigrate(&faultParent{}, &faultChild{}); err != nil {
		t.Fatal(err)
	}
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func count(t *testing.T, db *database.DB, model any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(model).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// createFamily writes a parent and its child in one transaction.
func createFamily(db *database.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		p := faultParent{Name: "p"}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		return tx.Create(&faultChild{ParentID: p.ID, Name: "c"}).Error
	})
}

// testDBFaults runs the wrapper's contract against a real driver.
func testDBFaults(t *testing.T, kind database.Driver, dsn string) {
	t.Run("the nth matching statement fails before it reaches the database", func(t *testing.T) {
		stmt := faulttest.FailOnCall(1, faulttest.ErrInjected)
		db := openFaultDB(t, kind, dsn, &faulttest.DBFaults{Statement: stmt, Match: touches("faulttest_children")})
		if err := createFamily(db); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("transaction = %v, want the injected fault", err)
		}
		if p, c := count(t, db, &faultParent{}), count(t, db, &faultChild{}); p != 0 || c != 0 {
			t.Fatalf("after a failed transaction: %d parents, %d children; want none", p, c)
		}
		if err := createFamily(db); err != nil {
			t.Fatalf("the next transaction = %v, want success", err)
		}
		if stmt.Calls() != 2 || stmt.Failures() != 1 {
			t.Fatalf("Statement: %d calls, %d failures; want 2, 1 (only child inserts count)", stmt.Calls(), stmt.Failures())
		}
	})

	t.Run("every statement counts without a match", func(t *testing.T) {
		stmt := faulttest.FailOnCall(2, faulttest.ErrInjected)
		db := openFaultDB(t, kind, dsn, &faulttest.DBFaults{Statement: stmt})
		if err := db.Create(&faultParent{Name: "one"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&faultParent{Name: "two"}).Error; !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("2nd statement = %v, want the injected fault", err)
		}
		if n := count(t, db, &faultParent{}); n != 1 {
			t.Fatalf("%d parents; want only the first", n)
		}
	})

	t.Run("a failed commit persists nothing and leaves the connection clean", func(t *testing.T) {
		commit := faulttest.FailOnce(faulttest.ErrInjected)
		db := openFaultDB(t, kind, dsn, &faulttest.DBFaults{Commit: commit})
		if err := createFamily(db); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("transaction = %v, want the commit fault", err)
		}
		if p, c := count(t, db, &faultParent{}), count(t, db, &faultChild{}); p != 0 || c != 0 {
			t.Fatalf("after a failed commit: %d parents, %d children; want none", p, c)
		}
		if err := createFamily(db); err != nil {
			t.Fatalf("the next transaction = %v, want success", err)
		}
		if p := count(t, db, &faultParent{}); p != 1 {
			t.Fatalf("%d parents after the retry, want 1", p)
		}
	})

	t.Run("a failed rollback still rolls back", func(t *testing.T) {
		rollback := faulttest.FailOnce(faulttest.ErrInjected)
		db := openFaultDB(t, kind, dsn, &faulttest.DBFaults{Rollback: rollback})
		tx := db.Begin()
		if err := tx.Create(&faultParent{Name: "p"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback().Error; !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("Rollback = %v, want the injected fault", err)
		}
		if n := count(t, db, &faultParent{}); n != 0 {
			t.Fatalf("%d parents after the rollback, want none", n)
		}
	})

	t.Run("a failed begin fails the transaction", func(t *testing.T) {
		db := openFaultDB(t, kind, dsn, &faulttest.DBFaults{Begin: faulttest.FailOnce(faulttest.ErrInjected)})
		if err := createFamily(db); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("transaction = %v, want the begin fault", err)
		}
		if err := createFamily(db); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a blocked statement ends with its context", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		stmt := faulttest.BlockUntil(release)
		db := openFaultDB(t, kind, dsn, &faulttest.DBFaults{Statement: stmt, Match: touches("faulttest_parents")})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		// No implicit transaction: its rollback error would bury the cause.
		session := db.Session(&gorm.Session{Context: ctx, SkipDefaultTransaction: true})
		go func() { done <- session.Create(&faultParent{Name: "p"}).Error }()
		<-stmt.Reached(1)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked insert = %v, want context.Canceled", err)
		}
	})
}

func TestSQLiteFaults(t *testing.T) {
	testDBFaults(t, database.DriverSQLite, filepath.Join(t.TempDir(), "faults.db"))
}

func TestConnectFaultFailsTheOpen(t *testing.T) {
	_, err := faulttest.OpenDB(database.DriverSQLite, filepath.Join(t.TempDir(), "c.db"),
		&faulttest.DBFaults{Connect: faulttest.FailAlways(faulttest.ErrInjected)})
	if !errors.Is(err, faulttest.ErrInjected) {
		t.Fatalf("OpenDB with every connect failing = %v, want the injected fault", err)
	}
	if _, err := faulttest.OpenDB("oracle", "", nil); err == nil {
		t.Fatal("OpenDB accepted an unsupported driver")
	}
}

// skippingDriver declines ExecContext (driver.ErrSkip), as go-sql-driver
// does for a statement with arguments: database/sql then prepares it.
type skippingDriver struct{ execs *atomic.Int32 }

func (d skippingDriver) Open(string) (driver.Conn, error) { return skippingConn(d), nil }

type skippingConn struct{ execs *atomic.Int32 }

func (c skippingConn) Prepare(string) (driver.Stmt, error) { return countingStmt(c), nil }
func (skippingConn) Close() error                          { return nil }
func (skippingConn) Begin() (driver.Tx, error)             { return nil, errors.New("no transactions") }
func (skippingConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, driver.ErrSkip
}

// preparingConn has no ExecContext at all.
type preparingConn struct{ execs *atomic.Int32 }

func (c preparingConn) Prepare(string) (driver.Stmt, error) { return countingStmt(c), nil }
func (preparingConn) Close() error                          { return nil }
func (preparingConn) Begin() (driver.Tx, error)             { return nil, errors.New("no transactions") }

type countingStmt struct{ execs *atomic.Int32 }

func (countingStmt) Close() error  { return nil }
func (countingStmt) NumInput() int { return -1 }
func (s countingStmt) Exec([]driver.Value) (driver.Result, error) {
	s.execs.Add(1)
	return driver.RowsAffected(1), nil
}
func (countingStmt) Query([]driver.Value) (driver.Rows, error) { return emptyRows{}, nil }

type emptyRows struct{}

func (emptyRows) Columns() []string         { return nil }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

type connFunc func() driver.Conn

func (f connFunc) Connect(context.Context) (driver.Conn, error) { return f(), nil }
func (connFunc) Driver() driver.Driver                          { return skippingDriver{} }

// TestAStatementIsCountedOnce: a statement the driver runs through the
// prepared-statement path (it declined ExecContext, or has none) hits the
// Statement fault once, not once per path.
func TestAStatementIsCountedOnce(t *testing.T) {
	for name, newConn := range map[string]func(*atomic.Int32) driver.Conn{
		"declines ExecContext": func(n *atomic.Int32) driver.Conn { return skippingConn{n} },
		"no ExecContext":       func(n *atomic.Int32) driver.Conn { return preparingConn{n} },
	} {
		t.Run(name, func(t *testing.T) {
			var execs atomic.Int32
			stmt := faulttest.FailOnCall(2, faulttest.ErrInjected)
			db := sql.OpenDB(faulttest.WrapConnector(connFunc(func() driver.Conn { return newConn(&execs) }), &faulttest.DBFaults{Statement: stmt}))
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("UPDATE t SET x = ?", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE t SET x = ?", 2); !errors.Is(err, faulttest.ErrInjected) {
				t.Fatalf("2nd statement = %v, want the injected fault", err)
			}
			if stmt.Calls() != 2 || execs.Load() != 1 {
				t.Fatalf("Statement calls %d, executed %d; want 2 calls and only the first executed", stmt.Calls(), execs.Load())
			}
		})
	}
}

// flakyPrepareConn declines ExecContext and fails its first Prepare.
type flakyPrepareConn struct {
	execs    *atomic.Int32
	prepares *atomic.Int32
}

func (c flakyPrepareConn) Prepare(string) (driver.Stmt, error) {
	if c.prepares.Add(1) == 1 {
		return nil, errors.New("prepare failed")
	}
	return countingStmt{c.execs}, nil
}
func (flakyPrepareConn) Close() error              { return nil }
func (flakyPrepareConn) Begin() (driver.Tx, error) { return nil, errors.New("no transactions") }
func (flakyPrepareConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, driver.ErrSkip
}

// TestAFailedPrepareLeavesNoMark: a declined statement whose prepare fails
// must not let the next identical statement skip its fault check.
func TestAFailedPrepareLeavesNoMark(t *testing.T) {
	var execs, prepares atomic.Int32
	stmt := faulttest.FailOnCall(2, faulttest.ErrInjected)
	c := flakyPrepareConn{&execs, &prepares}
	db := sql.OpenDB(faulttest.WrapConnector(connFunc(func() driver.Conn { return c }), &faulttest.DBFaults{Statement: stmt}))
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("UPDATE t SET x = ?", 1); err == nil || errors.Is(err, faulttest.ErrInjected) {
		t.Fatalf("1st statement = %v, want the prepare failure", err)
	}
	// The same SQL through an explicit prepared statement: a mark left by the
	// failed prepare would let it skip its fault check.
	prepared, err := db.Prepare("UPDATE t SET x = ?")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prepared.Close() }()
	if _, err := prepared.Exec(1); !errors.Is(err, faulttest.ErrInjected) {
		t.Fatalf("2nd statement = %v, want the injected fault (call 2)", err)
	}
	if execs.Load() != 0 || stmt.Calls() != 2 {
		t.Fatalf("executed %d, Statement calls %d; want 0 and 2", execs.Load(), stmt.Calls())
	}
}
