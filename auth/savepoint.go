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
// is set. The rollback runs on its own bounded context; when it fails, the
// error carries errTransactionLost.
//
// bestEffort is a write that must not hold, or break, the caller's
// transaction (an audit event): ctx's deadline bounds it, but inside a
// transaction the database enforces that deadline (statement_timeout on
// PostgreSQL, the lock wait timeouts on MySQL), not a context
// cancellation: cancelling a statement in flight drops its connection, and
// the caller's transaction with it. Each statement gets its own session:
// GORM keeps a statement's error on the session that ran it.
func insertUnderSavepoint(ctx context.Context, db *gorm.DB, savepoint string, bestEffort bool, insert func(*gorm.DB) error) error {
	if committer, ok := db.Statement.ConnPool.(gorm.TxCommitter); !ok || committer == nil {
		return insert(db.WithContext(ctx))
	}
	stmtCtx := ctx
	var limit time.Duration
	if bestEffort {
		if deadline, ok := ctx.Deadline(); ok {
			limit = max(time.Until(deadline), time.Millisecond)
		}
		stmtCtx = context.WithoutCancel(ctx)
	}
	session := func() *gorm.DB { return db.WithContext(stmtCtx) }
	cleanup := func() (*gorm.DB, context.CancelFunc) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), savepointCleanupTimeout)
		return db.WithContext(cctx), cancel
	}
	if err := session().SavePoint(savepoint).Error; err != nil {
		return err
	}
	restore := func(*gorm.DB) error { return nil }
	err := error(nil)
	if limit > 0 {
		restore, err = boundStatements(session(), limit)
	}
	if err == nil {
		err = insert(session())
	}
	if err == nil {
		if err = restore(session()); err == nil {
			return nil
		}
	}
	tx, cancel := cleanup()
	defer cancel()
	if rerr := tx.RollbackTo(savepoint).Error; rerr != nil {
		return errors.Join(err, rerr, errTransactionLost)
	}
	// A rollback to the savepoint undoes PostgreSQL's SET LOCAL, not
	// MySQL's session variables.
	if rerr := restore(tx); rerr != nil {
		return errors.Join(err, rerr, errTransactionLost)
	}
	return err
}

// boundStatements makes the database itself end db's next statements once
// limit has passed, and returns what puts the previous bound back. SQLite
// needs none: its one connection is the caller's, so nothing else holds a
// lock the insert could wait on.
func boundStatements(db *gorm.DB, limit time.Duration) (restore func(*gorm.DB) error, err error) {
	switch db.Name() {
	case "postgres":
		var prev string
		if err := db.Raw("SELECT current_setting('statement_timeout')").Scan(&prev).Error; err != nil {
			return nil, err
		}
		ms := fmt.Sprintf("%dms", max(limit.Milliseconds(), 1))
		if err := db.Exec("SELECT set_config('statement_timeout', ?, true)", ms).Error; err != nil {
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
		secs := int64(math.Ceil(limit.Seconds()))
		if err := db.Exec(fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d, lock_wait_timeout = %d", secs, secs)).Error; err != nil {
			return nil, err
		}
		return func(tx *gorm.DB) error {
			return tx.Exec(fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d, lock_wait_timeout = %d", prev.Innodb, prev.Lock)).Error
		}, nil
	default:
		return func(*gorm.DB) error { return nil }, nil
	}
}
