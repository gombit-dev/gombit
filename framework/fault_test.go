package framework

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Fault tests for App.Tx: INV-1 (a failed transaction leaves no partial
// state) and INV-5 (infrastructure failure is an error, not a panic), with
// faults injected at the database/sql driver under the real GORM stack.

type faultTxParent struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

func (faultTxParent) TableName() string { return "fault_tx_parents" }

type faultTxChild struct {
	ID       uint `gorm:"primaryKey"`
	ParentID uint
	Name     string
}

func (faultTxChild) TableName() string { return "fault_tx_children" }

// faultDBs are SQLite, plus PostgreSQL and MySQL under the integration tag
// (fault_integration_test.go).
var faultDBs = []faulttest.TestDB{faulttest.SQLiteDB()}

// newFaultApp is an App over a database wrapped with faults, with fresh
// tables. Every injector in faults is disarmed while the tables are made
// and armed for the test.
func newFaultApp(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults) *App {
	t.Helper()
	faults.Disarm()
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		faults.Disarm()
		// Bounded: a transaction a failed test left open may hold the
		// only SQLite connection.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.WithContext(ctx).Migrator().DropTable(&faultTxChild{}, &faultTxParent{})
		_ = db.Close()
	})
	if err := db.Migrator().DropTable(&faultTxChild{}, &faultTxParent{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&faultTxParent{}, &faultTxChild{}); err != nil {
		t.Fatal(err)
	}
	faults.Arm()
	return newTestApp(t, WithDatabase(db))
}

// writeFamily inserts a parent and its child in one App.Tx.
func writeFamily(ctx context.Context, app *App) error {
	return app.Tx(ctx, func(tx *gorm.DB) error {
		p := faultTxParent{Name: "parent"}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		return tx.Create(&faultTxChild{ParentID: p.ID, Name: "child"}).Error
	})
}

// assertNoFamily fails t unless neither table holds a row and no
// transaction was left open.
func assertNoFamily(t *testing.T, app *App) {
	t.Helper()
	faulttest.Idle(t, app.Database())
	var parents, children int64
	if err := app.DB().Model(&faultTxParent{}).Count(&parents).Error; err != nil {
		t.Fatal(err)
	}
	if err := app.DB().Model(&faultTxChild{}).Count(&children).Error; err != nil {
		t.Fatal(err)
	}
	if parents != 0 || children != 0 {
		t.Fatalf("partial state after a failed transaction: %d parents, %d children; want none", parents, children)
	}
}

// TestFault_Database_TransactionRollback: BEGIN / INSERT parent ✓ /
// INSERT child ✗ / ROLLBACK leaves neither row, and the next transaction
// goes through.
func TestFault_Database_TransactionRollback(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailOnce(faulttest.ErrInjected),
			Match:     faulttest.Inserts("fault_tx_children"),
		})
		if err := writeFamily(context.Background(), app); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("Tx = %v, want the injected statement fault", err)
		}
		assertNoFamily(t, app)
		if err := writeFamily(context.Background(), app); err != nil {
			t.Fatalf("the next Tx = %v, want success once the fault passed", err)
		}
	})
}

// TestFault_Database_CommitFailure: a commit that fails is reported, never
// taken for success, and persists nothing. (It catches `tx.Commit() //
// error ignored; return nil`.)
func TestFault_Database_CommitFailure(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		commit := faulttest.FailOnce(faulttest.ErrInjected)
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{Commit: commit})
		if err := writeFamily(context.Background(), app); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("Tx = %v, want the commit fault", err)
		}
		if commit.Failures() != 1 {
			t.Fatalf("commit failures = %d, want 1: the Tx did not reach its commit", commit.Failures())
		}
		assertNoFamily(t, app)
		if err := writeFamily(context.Background(), app); err != nil {
			t.Fatalf("the next Tx = %v, want success", err)
		}
	})
}

// TestFault_Database_Cancellation: a context canceled while a statement is
// in flight ends the transaction with context.Canceled and no partial rows.
func TestFault_Database_Cancellation(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer close(release)
		child := faulttest.BlockUntil(release)
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{Statement: child, Match: faulttest.Inserts("fault_tx_children")})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- writeFamily(ctx, app) }()
		<-child.Reached(1) // the parent is inserted, the child insert is in flight
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Tx = %v, want context.Canceled", err)
		}
		assertNoFamily(t, app)
	})
}

// retryableFailure is kind's own retryable transaction error, as its driver
// returns it, and a check that recognizes it: a serialization failure on
// Postgres (40001), a deadlock on MySQL (1213), a busy database on SQLite.
func retryableFailure(kind database.Driver) (recognize func(error) bool, failure error) {
	switch kind {
	case database.DriverPostgres:
		return func(err error) bool {
			var pgErr *pgconn.PgError
			return errors.As(err, &pgErr) && pgErr.Code == "40001"
		}, &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"}
	case database.DriverMySQL:
		return func(err error) bool {
			var myErr *mysql.MySQLError
			return errors.As(err, &myErr) && myErr.Number == 1213
		}, &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	default:
		return func(err error) bool {
			var liteErr sqlite3.Error
			return errors.As(err, &liteErr) && liteErr.Code == sqlite3.ErrBusy
		}, sqlite3.Error{Code: sqlite3.ErrBusy}
	}
}

// TestFault_Database_RetryableError: the driver's retryable transaction
// error is returned as a terminal, classifiable error: App.Tx does not retry
// it (fn runs once), and the caller can recognize it.
func TestFault_Database_RetryableError(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		recognize, failure := retryableFailure(kind)
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailAlways(failure),
			Match:     faulttest.Inserts("fault_tx_children"),
		})
		runs := 0
		err := app.Tx(context.Background(), func(tx *gorm.DB) error {
			runs++
			p := faultTxParent{Name: "parent"}
			if err := tx.Create(&p).Error; err != nil {
				return err
			}
			return tx.Create(&faultTxChild{ParentID: p.ID}).Error
		})
		if !recognize(err) {
			t.Fatalf("Tx = %v, want the driver's retryable error, recognizable", err)
		}
		if runs != 1 {
			t.Fatalf("fn ran %d times, want 1: App.Tx does not retry", runs)
		}
		assertNoFamily(t, app)
	})
}

// TestFault_Database_PanicRollsBack: a panic in fn rolls the transaction
// back and propagates, even when the rollback itself reports an error.
func TestFault_Database_PanicRollsBack(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		rollback := faulttest.FailOnce(faulttest.ErrInjected)
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{Rollback: rollback})
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the panic in fn did not propagate")
				}
			}()
			_ = app.Tx(context.Background(), func(tx *gorm.DB) error {
				if err := tx.Create(&faultTxParent{Name: "parent"}).Error; err != nil {
					return err
				}
				panic("handler bug")
			})
		}()
		if rollback.Calls() != 1 {
			t.Fatalf("rollback calls = %d, want 1", rollback.Calls())
		}
		assertNoFamily(t, app)
	})
}

// TestFault_Database_Unavailable: every statement failing is an error from
// App.Tx, not a panic, and nothing persists.
func TestFault_Database_Unavailable(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailAlways(faulttest.ErrInjected),
			Match:     faulttest.Inserts("fault_tx_"),
		})
		if err := writeFamily(context.Background(), app); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("Tx = %v, want the injected fault", err)
		}
		assertNoFamily(t, app)
	})
}
