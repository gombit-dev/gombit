package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
)

// Inserts the trail and the grant helpers make inside a caller's
// transaction (a *gorm.DB the caller opened a transaction on).

// savepointCleanupTimeout bounds rolling back to a savepoint after a failed
// insert. It runs on a context of its own: the insert's may be the one that
// just expired.
const savepointCleanupTimeout = 5 * time.Second

// The savepoint statements are written out, not GORM's SavePoint and
// RollbackTo: the PostgreSQL and SQLite dialectors run those on a cloned
// session and return nil, so a SAVEPOINT that failed would look made, and a
// ROLLBACK TO it would then abort the transaction unseen. The SQL is the
// same on the three databases; savepoint is a constant name.

// errTransactionLost is an insert inside a caller's transaction that failed
// when rolling back to its savepoint failed too: the caller's transaction
// is unusable (on PostgreSQL it stays aborted; a dropped connection takes
// it with it), and the caller must be told, not left to find out at commit.
var errTransactionLost = errors.New("auth: the caller's transaction could not be recovered after a failed insert")

// insertSafely runs insert on db, under insertUnderSavepoint's savepoint
// when db is in a transaction, with ctx as the caller gave it.
func insertSafely(ctx context.Context, db *gorm.DB, savepoint string, insert func(*gorm.DB) error) error {
	return insertUnderSavepoint(ctx, db, savepoint, false, insert)
}

// insertUnderSavepoint runs insert on db. Outside a transaction it is one
// statement on ctx. Inside one it runs under an explicit savepoint, rolled
// back to if insert fails, so a failed statement does not abort the
// caller's transaction (PostgreSQL); not db.Transaction, which writes
// straight into the outer transaction when GORM's DisableNestedTransaction
// is set. When the rollback fails too, the error carries
// errTransactionLost. Each statement gets its own session: GORM keeps a
// statement's error on the session that ran it.
//
// bestEffort is a write that must not hold the caller's transaction past
// ctx's deadline (an audit event): one absolute budget, from now to that
// deadline, bounds all of it. The savepoint, the setup and the insert run
// under a client-side deadline at the end of its first part (workDeadline);
// the database's own limit (statement_timeout on PostgreSQL, the lock wait
// timeouts on MySQL), computed from what is left just before it is set,
// ends a slow statement a margin before that, so a lock wait is the
// database's to end and the transaction survives it. A stall the database
// cannot end (an unresponsive connection) meets the client deadline, whose
// cancellation drops the connection: the caller's transaction is lost, and
// the rollback that cannot run reports it (errTransactionLost). The rest of
// the budget, after workDeadline, is the rollback's and the restore's.
func insertUnderSavepoint(ctx context.Context, db *gorm.DB, savepoint string, bestEffort bool, insert func(*gorm.DB) error) error {
	if committer, ok := db.Statement.ConnPool.(gorm.TxCommitter); !ok || committer == nil {
		return insert(db.WithContext(ctx))
	}
	workCtx, cleanupCtx := ctx, context.WithoutCancel(ctx)
	var margin time.Duration
	var workDeadline time.Time
	cleanupDeadline := time.Now().Add(savepointCleanupTimeout)
	if deadline, ok := ctx.Deadline(); bestEffort && ok {
		budget := time.Until(deadline)
		reserve := budget / 5
		margin = reserve / 2
		workDeadline = deadline.Add(-reserve)
		cleanupDeadline = deadline
		var cancel context.CancelFunc
		workCtx, cancel = context.WithDeadline(ctx, workDeadline)
		defer cancel()
	}
	cleanupCtx, cancelCleanup := context.WithDeadline(cleanupCtx, cleanupDeadline)
	defer cancelCleanup()

	if err := db.WithContext(workCtx).Exec("SAVEPOINT " + savepoint).Error; err != nil {
		// No savepoint to go back to: whatever the failure did to the
		// transaction (PostgreSQL aborts it; a cancelled statement drops its
		// connection) cannot be undone here.
		return errors.Join(err, errTransactionLost)
	}
	restore := func(*gorm.DB) error { return nil }
	var err error
	if !workDeadline.IsZero() {
		var installed func(*gorm.DB) error
		if installed, err = boundStatements(db.WithContext(workCtx), workDeadline, margin); err == nil {
			restore = installed
		}
	}
	if err == nil {
		err = insert(db.WithContext(workCtx))
	}
	tx := db.WithContext(cleanupCtx)
	if err == nil {
		if err = restore(tx); err == nil {
			return nil
		}
	}
	if rerr := tx.Exec("ROLLBACK TO SAVEPOINT " + savepoint).Error; rerr != nil {
		return errors.Join(err, rerr, errTransactionLost)
	}
	// A rollback to the savepoint undoes PostgreSQL's SET LOCAL, not
	// MySQL's session variables.
	if rerr := restore(tx); rerr != nil {
		return errors.Join(err, rerr, errTransactionLost)
	}
	return err
}

// boundStatements makes the database itself end db's next statements a
// margin before deadline, and returns what puts the previous bound back.
// The limit is computed from the time left as it is set, not before. A
// limit the database cannot express (MySQL counts whole seconds) is not
// set: the client deadline is the only bound then. SQLite needs none: its
// one connection is the caller's, so nothing else holds a lock the insert
// could wait on.
func boundStatements(db *gorm.DB, deadline time.Time, margin time.Duration) (restore func(*gorm.DB) error, err error) {
	noop := func(*gorm.DB) error { return nil }
	switch db.Name() {
	case "postgres":
		var prev string
		if err := db.Raw("SELECT current_setting('statement_timeout')").Scan(&prev).Error; err != nil {
			return nil, err
		}
		limit := time.Until(deadline) - margin
		if limit < time.Millisecond {
			return noop, nil
		}
		if err := db.Exec("SELECT set_config('statement_timeout', ?, true)", fmt.Sprintf("%dms", limit.Milliseconds())).Error; err != nil {
			return nil, err
		}
		return func(tx *gorm.DB) error {
			return tx.Exec("SELECT set_config('statement_timeout', ?, true)", prev).Error
		}, nil
	case "mysql":
		var prev struct{ Innodb, Lock int64 }
		if err := db.Raw("SELECT @@SESSION.innodb_lock_wait_timeout AS innodb, @@SESSION.lock_wait_timeout AS `lock`").Scan(&prev).Error; err != nil {
			return nil, err
		}
		secs := int64(math.Floor((time.Until(deadline) - margin).Seconds()))
		if secs < 1 {
			return noop, nil
		}
		if err := db.Exec(fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d, lock_wait_timeout = %d", secs, secs)).Error; err != nil {
			return nil, err
		}
		return func(tx *gorm.DB) error {
			return tx.Exec(fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d, lock_wait_timeout = %d", prev.Innodb, prev.Lock)).Error
		}, nil
	default:
		return noop, nil
	}
}
