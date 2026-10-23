//go:build integration

package auth_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// TestAuditStallInTransactionPostgres: inside a caller's transaction, a
// grant's audit insert really stalls in PostgreSQL (another connection holds
// the audit table's lock) until the audit deadline cancels it. PostgreSQL
// then holds the transaction aborted until ROLLBACK TO SAVEPOINT, which must
// run on a context of its own, not the one that just expired: the caller's
// next statement and its commit succeed, and the grant persists.
func TestAuditStallInTransactionPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -auth.postgres-dsn to run Postgres auth integration tests")
	}
	db := openDriver(t, config.DatabaseDriverPostgres, *postgresDSN)
	runAuditStallInTransaction(t, db, func() (release func()) {
		lock := db.Begin()
		if err := lock.Exec("LOCK TABLE auth_audit_events IN ACCESS EXCLUSIVE MODE").Error; err != nil {
			t.Fatal(err)
		}
		return func() { _ = lock.Rollback().Error }
	})
}

// TestAuditStallInTransactionMySQL: the same on MySQL, where the stall is a
// table lock another connection holds, and the database ends the insert's
// wait (lock_wait_timeout) without touching the caller's connection.
func TestAuditStallInTransactionMySQL(t *testing.T) {
	if *mysqlDSN == "" {
		t.Skip("set -auth.mysql-dsn to run MySQL auth integration tests")
	}
	db := openDriver(t, config.DatabaseDriverMySQL, *mysqlDSN)
	runAuditStallInTransaction(t, db, func() (release func()) {
		sqlDB, err := db.DB.DB()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := sqlDB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "LOCK TABLES auth_audit_events WRITE"); err != nil {
			t.Fatal(err)
		}
		return func() {
			_, _ = conn.ExecContext(context.Background(), "UNLOCK TABLES")
			_ = conn.Close()
		}
	})
}

func runAuditStallInTransaction(t *testing.T, db *database.DB, lockAuditTable func() (release func())) {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	auth.SetAuditTimeout(t, 200*time.Millisecond)
	ctx := context.Background()
	n := time.Now().UnixNano()
	user := auth.User{Email: fmt.Sprintf("stall-tx-%d@example.com", n), PasswordHash: "x"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	group, err := auth.EnsureGroup(ctx, db.DB, fmt.Sprintf("stall-tx-%d", n))
	if err != nil {
		t.Fatal(err)
	}

	unlock := lockAuditTable()
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	defer release()

	err = db.Transaction(func(tx *gorm.DB) error {
		if err := auth.AddUserToGroup(ctx, tx, &user, &group); err != nil {
			return err
		}
		// A statement after the timed-out audit insert, in the same transaction.
		return tx.Model(&auth.User{}).Where("id = ?", user.ID).Update("is_superuser", false).Error
	})
	release()
	if err != nil {
		t.Fatalf("the caller's transaction after a timed-out audit insert = %v", err)
	}
	var granted int64
	db.Table("auth_user_groups").Where("user_id = ? AND group_id = ?", user.ID, group.ID).Count(&granted)
	if granted != 1 {
		t.Fatal("the grant did not commit")
	}
}
