package auth_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_Database_AuditInsertStallIsBounded: the audit insert stalls in
// the driver and never comes back. Recording is best effort, and the
// operation it records has committed: the operation answers once the audit
// write's own deadline passes, not never, even with a context that has no
// deadline of its own.
func TestFault_Database_AuditInsertStallIsBounded(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		auth.SetAuditTimeout(t, 100*time.Millisecond)
		never := make(chan struct{})
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.BlockUntil(never),
			Match:     faulttest.Inserts("auth_audit_events"),
		})
		done := make(chan error, 1)
		go func() {
			_, err := f.svc.Register(context.Background(), "stalled-audit@example.com", "correct horse battery")
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Register with a stalled audit insert = %v, want the registration", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Register waited on a stalled audit insert past its deadline")
		}
		var n int64
		if err := f.db.Model(&auth.User{}).Where("email = ?", "stalled-audit@example.com").Count(&n).Error; err != nil || n != 1 {
			t.Fatalf("the committed user: %d rows, %v", n, err)
		}
	})
}

// grantInTransaction runs a permission grant in a caller's transaction over
// f's database, then another statement, and commits; it returns the grant's
// error and the transaction's, and how long it all took.
func grantInTransaction(t *testing.T, f rotationFixture, faults *faulttest.DBFaults) (took time.Duration, grantErr, txErr error) {
	t.Helper()
	faults.Disarm()
	ctx := context.Background()
	perm, err := auth.EnsurePermission(ctx, f.db.DB, fmt.Sprintf("fault.grant.%d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatal(err)
	}
	faults.Arm()
	start := time.Now()
	txErr = f.db.Transaction(func(tx *gorm.DB) error {
		grantErr = auth.GrantPermissionToUser(ctx, tx, &f.user, &perm)
		if grantErr != nil {
			return grantErr
		}
		return tx.Model(&auth.User{}).Where("id = ?", f.user.ID).Update("is_superuser", false).Error
	})
	took = time.Since(start)
	faults.Disarm()
	return took, grantErr, txErr
}

// installsAuditTimeout matches the statements that install the database's
// own deadline for an audit insert in a transaction.
func installsAuditTimeout(query string) bool {
	return strings.Contains(query, "statement_timeout") || strings.Contains(query, "@@SESSION")
}

// TestFault_Database_AuditTimeoutSetupFails: installing the database's
// deadline for an in-transaction audit insert fails at once. That loses the
// event (logged), nothing more: no panic, the grant and the caller's
// transaction go on and commit.
func TestFault_Database_AuditTimeoutSetupFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		if kind == database.DriverSQLite {
			t.Skip("SQLite installs no deadline")
		}
		faults := &faulttest.DBFaults{Statement: faulttest.FailOnce(faulttest.ErrInjected), Match: installsAuditTimeout}
		f := newRotationFixture(t, kind, dsn, faults)
		_, grantErr, txErr := grantInTransaction(t, f, faults)
		if grantErr != nil || txErr != nil {
			t.Fatalf("grant = %v, transaction = %v; a failed timeout setup must only lose the event", grantErr, txErr)
		}
		if faults.Statement.Failures() != 1 {
			t.Fatal("the timeout setup was never attempted")
		}
	})
}

// TestFault_Database_AuditTimeoutSetupStalls: the statement that installs
// the deadline stalls. The setup runs under the audit deadline itself, so
// the grant returns within it, not never.
func TestFault_Database_AuditTimeoutSetupStalls(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		if kind == database.DriverSQLite {
			t.Skip("SQLite installs no deadline")
		}
		auth.SetAuditTimeout(t, 100*time.Millisecond)
		never := make(chan struct{})
		faults := &faulttest.DBFaults{Statement: faulttest.BlockUntil(never), Match: installsAuditTimeout}
		f := newRotationFixture(t, kind, dsn, faults)
		took, _, _ := grantInTransaction(t, f, faults)
		if took > 5*time.Second {
			t.Fatalf("a stalled timeout setup held the grant %v", took)
		}
	})
}

// TestFault_Database_AuditSavepointStalls: the audit insert's SAVEPOINT
// stalls. It is bounded by the audit deadline too; with no savepoint to go
// back to, the grant reports the transaction unusable instead of hiding it.
func TestFault_Database_AuditSavepointStalls(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		auth.SetAuditTimeout(t, 100*time.Millisecond)
		never := make(chan struct{})
		faults := &faulttest.DBFaults{Statement: faulttest.BlockUntil(never), Match: func(q string) bool {
			return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(q)), "SAVEPOINT") && strings.Contains(q, "gombit_audit_event")
		}}
		f := newRotationFixture(t, kind, dsn, faults)
		took, grantErr, txErr := grantInTransaction(t, f, faults)
		if took > 5*time.Second {
			t.Fatalf("a stalled SAVEPOINT held the grant %v", took)
		}
		if grantErr == nil || !strings.Contains(grantErr.Error(), "could not be recovered") || txErr == nil {
			t.Fatalf("grant = %v, transaction = %v; want the transaction reported lost", grantErr, txErr)
		}
	})
}

// TestFault_Database_AuditBudgetIsAbsolute: a slow but successful
// SAVEPOINT spends most of the audit budget, then the insert stalls. The
// insert gets what is left of the one budget, not a fresh one: the grant
// returns within the budget (plus scheduling slack), not after it again.
func TestFault_Database_AuditBudgetIsAbsolute(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		const budget = 300 * time.Millisecond
		auth.SetAuditTimeout(t, budget)
		never := make(chan struct{})
		faults := &faulttest.DBFaults{
			// Call 1: the audit SAVEPOINT, slow; call 2: the insert, stalled.
			Statement: faulttest.Sequence(faulttest.Wait(200*time.Millisecond), faulttest.Block(never)),
			Match: func(q string) bool {
				q = strings.TrimSpace(q)
				return (strings.HasPrefix(strings.ToUpper(q), "SAVEPOINT") && strings.Contains(q, "gombit_audit_event")) ||
					faulttest.Inserts("auth_audit_events")(q)
			},
		}
		f := newRotationFixture(t, kind, dsn, faults)
		took, _, _ := grantInTransaction(t, f, faults)
		if took > budget+500*time.Millisecond {
			t.Fatalf("the grant took %v: the insert got a budget the savepoint had already spent (%v in all)", took, budget)
		}
	})
}
