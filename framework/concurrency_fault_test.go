package framework

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Concurrency + fault tests: a fault at a dangerous boundary while another
// operation runs against the same data. Interleavings are pinned with
// channels and the fault injectors (Reached, Block, BlockThenFail), never
// with sleeps, so each scenario has one expected outcome, documented on it.

// seedParent inserts one faultTxParent outside the fault sequence.
func seedParent(t *testing.T, app *App, faults *faulttest.DBFaults, name string) uint {
	t.Helper()
	faults.Disarm()
	defer faults.Arm()
	p := faultTxParent{Name: name}
	if err := app.DB().Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// await fails t unless ch delivers within 5s.
func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", what)
		var zero T
		return zero
	}
}

// TestFault_Concurrency_ConflictingWriters: two transactions update the
// same row; the first holds its row lock (or, on SQLite, the connection)
// until its COMMIT fails. Expected outcome: one winner. The second writer's
// update commits, and nothing of the first survives, not its update and not
// the child row it inserted in the same transaction.
func TestFault_Concurrency_ConflictingWriters(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		releaseA := make(chan struct{})
		defer func() {
			select {
			case <-releaseA:
			default:
				close(releaseA)
			}
		}()
		commit := faulttest.Sequence(faulttest.BlockThenFail(releaseA, faulttest.ErrInjected))
		updates := faulttest.Sequence() // never fails: marks each writer's UPDATE reaching the driver
		faults := &faulttest.DBFaults{Commit: commit, Statement: updates, Match: func(q string) bool {
			return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(q)), "UPDATE") && strings.Contains(q, "fault_tx_parents")
		}}
		app := newFaultApp(t, kind, dsn, faults)
		id := seedParent(t, app, faults, "initial")

		write := func(name string) error {
			return app.Tx(context.Background(), func(tx *gorm.DB) error {
				if err := tx.Model(&faultTxParent{}).Where("id = ?", id).Update("name", name).Error; err != nil {
					return err
				}
				return tx.Create(&faultTxChild{ParentID: id, Name: name + "-child"}).Error
			})
		}
		a := make(chan error, 1)
		go func() { a <- write("A") }()
		<-commit.Reached(1) // A has written and is committing, row lock held
		b := make(chan error, 1)
		go func() { b <- write("B") }()
		if kind != database.DriverSQLite {
			// B's UPDATE is on its way to the row A has locked; on SQLite, B
			// cannot even begin while A holds the only connection.
			<-updates.Reached(2)
		}
		close(releaseA) // A's commit fails: it rolls back, and B gets the row

		if err := await(t, "writer A", a); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("writer A = %v, want its commit failure", err)
		}
		if err := await(t, "writer B", b); err != nil {
			t.Fatalf("writer B = %v, want success once A rolled back", err)
		}
		var p faultTxParent
		if err := app.DB().First(&p, id).Error; err != nil {
			t.Fatal(err)
		}
		if p.Name != "B" {
			t.Fatalf("row name = %q, want B (the surviving writer)", p.Name)
		}
		var children []faultTxChild
		if err := app.DB().Where("parent_id = ?", id).Find(&children).Error; err != nil {
			t.Fatal(err)
		}
		if len(children) != 1 || children[0].Name != "B-child" {
			t.Fatalf("children = %+v, want only B's", children)
		}
		faulttest.Idle(t, app.Database())
	})
}

// TestFault_Concurrency_CancelDuringCommit: the caller's context is
// canceled while its COMMIT is in the driver. Expected outcome: a
// consistent answer, never an error for a write that persisted nor success
// for one that did not. Which answer depends on the driver, and is fixed
// for each: database/sql has already marked the transaction done, so the
// cancellation cannot roll it back from outside; SQLite and MySQL complete
// the commit (success, the row is there), while pgx checks the
// transaction's context before sending COMMIT and aborts it (the
// cancellation is returned, nothing persisted). The consistency check is
// the invariant; the per-driver outcome records how each driver behaves
// today (it rests on database/sql marking a transaction done before the
// driver's Commit runs), so a change there shows up here as a changed but
// still consistent outcome.
func TestFault_Concurrency_CancelDuringCommit(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		commit := faulttest.Sequence(faulttest.Block(release))
		app := newFaultApp(t, kind, dsn, &faulttest.DBFaults{Commit: commit})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			result <- app.Tx(ctx, func(tx *gorm.DB) error {
				return tx.Create(&faultTxParent{Name: "racing"}).Error
			})
		}()
		<-commit.Reached(1) // COMMIT is in the driver
		cancel()
		close(release)

		err := await(t, "the transaction", result)
		var n int64
		if cerr := app.DB().Model(&faultTxParent{}).Where("name = ?", "racing").Count(&n).Error; cerr != nil {
			t.Fatal(cerr)
		}
		if (err == nil) != (n == 1) {
			t.Fatalf("inconsistent outcome: App.Tx = %v but %d rows persisted", err, n)
		}
		if kind == database.DriverPostgres {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("App.Tx on Postgres = %v, want pgx to abort the COMMIT with context.Canceled", err)
			}
		} else if err != nil {
			t.Fatalf("App.Tx = %v; this driver completes a commit already under way", err)
		}
		faulttest.Idle(t, app.Database())
	})
}
