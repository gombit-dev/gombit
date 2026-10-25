//go:build integration

package auth_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
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
	runAuditStallInTransaction(t, db, 200*time.Millisecond, func() (release func()) {
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
	// MySQL's lock wait timeouts count whole seconds: a budget whose work
	// window leaves at least one, so the database ends the wait.
	runAuditStallInTransaction(t, db, 3*time.Second, func() (release func()) {
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

func runAuditStallInTransaction(t *testing.T, db *database.DB, budget time.Duration, lockAuditTable func() (release func())) {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	auth.SetAuditTimeout(t, budget)
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

// TestAuditNetworkStallInTransactionPostgres: the audit insert's bytes are
// sent and the connection stops answering (a TCP proxy holds them); no
// database setting can end that. The grant still returns within the audit
// budget, and, the connection lost to the cancellation, reports the
// caller's transaction lost instead of calling the audit failure harmless.
func TestAuditNetworkStallInTransactionPostgres(t *testing.T) {
	if *postgresDSN == "" {
		t.Skip("set -auth.postgres-dsn to run Postgres auth integration tests")
	}
	upstream, err := faulttest.PostgresHostPort(*postgresDSN)
	if err != nil {
		t.Fatal(err)
	}
	proxy := faulttest.NewTCPProxy(t, upstream)
	dsn, err := faulttest.PostgresDSNVia(*postgresDSN, proxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Heal() }()
	const budget = 300 * time.Millisecond
	auth.SetAuditTimeout(t, budget)
	// The audit insert's arrival holds the proxy: its bytes go out, and no
	// answer comes back.
	faults := &faulttest.DBFaults{
		Statement: faulttest.Sequence(faulttest.Do(proxy.Hold)),
		Match:     faulttest.Inserts("auth_audit_events"),
	}
	f := newRotationFixture(t, database.DriverPostgres, dsn, faults)
	took, grantErr, txErr := grantInTransaction(t, f, faults)
	_ = proxy.Heal()
	if took > budget+2*time.Second {
		t.Fatalf("a network stall held the grant %v, past its %v budget", took, budget)
	}
	if grantErr == nil || !strings.Contains(grantErr.Error(), "could not be recovered") || txErr == nil {
		t.Fatalf("grant = %v, transaction = %v; want the lost transaction reported", grantErr, txErr)
	}
}
