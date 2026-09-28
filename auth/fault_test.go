package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Fault tests for refresh-token rotation, which revokes the presented token
// and issues its replacement in one transaction: a failure after the revoke
// must not leave the old token revoked with no replacement (the user would
// be logged out by an infrastructure blip).

// faultDBs are SQLite, plus PostgreSQL and MySQL under the integration tag.
var faultDBs = []faulttest.TestDB{faulttest.SQLiteDB()}

// rotationFixture is an auth service over a faulted database with one
// user holding one refresh token.
type rotationFixture struct {
	db      *database.DB
	svc     *auth.Service
	user    auth.User
	refresh string
}

func newRotationFixture(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults) rotationFixture {
	t.Helper()
	faults.Disarm()
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	dropAuthTables := func() {
		// Bounded: a transaction a failed test left open may hold the only
		// SQLite connection.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m := db.WithContext(ctx).Migrator()
		_ = m.DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
		_ = m.DropTable(auth.Models()...)
	}
	t.Cleanup(func() {
		faults.Disarm()
		dropAuthTables()
		_ = db.Close()
	})
	dropAuthTables()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	svc, err := auth.NewService(db.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, err := svc.Register(ctx, "rotate@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := svc.IssueTokens(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	faults.Arm()
	return rotationFixture{db: db, svc: svc, user: user, refresh: pair.RefreshToken}
}

// assertUnrotated fails t unless the user still has exactly their one
// refresh token, unrevoked, and it still rotates.
func (f rotationFixture) assertUnrotated(t *testing.T) {
	t.Helper()
	faulttest.Idle(t, f.db)
	var tokens []auth.RefreshToken
	if err := f.db.Where("user_id = ?", f.user.ID).Find(&tokens).Error; err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].RevokedAt != nil {
		t.Fatalf("after a failed rotation: %d tokens (first revoked: %v); want the one original, unrevoked",
			len(tokens), len(tokens) > 0 && tokens[0].RevokedAt != nil)
	}
	next, err := f.svc.RotateRefresh(context.Background(), f.refresh)
	if err != nil || next.RefreshToken == "" {
		t.Fatalf("rotating again = %v; the session did not survive the failed rotation", err)
	}
}

// insertsRefreshToken matches the replacement token's INSERT.
var insertsRefreshToken = faulttest.Inserts("refresh_tokens")

// TestFault_Database_RefreshRotationAtomic: the replacement token's insert
// fails after the old token was revoked in the same transaction; the revoke
// rolls back with it.
func TestFault_Database_RefreshRotationAtomic(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailOnce(faulttest.ErrInjected),
			Match:     insertsRefreshToken,
		})
		if _, err := f.svc.RotateRefresh(context.Background(), f.refresh); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("RotateRefresh = %v, want the injected fault", err)
		}
		f.assertUnrotated(t)
	})
}

// TestFault_Database_RefreshRotationCommitFailure: a rotation whose commit
// fails issues nothing and revokes nothing.
func TestFault_Database_RefreshRotationCommitFailure(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{Commit: faulttest.FailOnce(faulttest.ErrInjected)})
		if _, err := f.svc.RotateRefresh(context.Background(), f.refresh); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("RotateRefresh = %v, want the commit fault", err)
		}
		f.assertUnrotated(t)
	})
}

// TestFault_Database_RefreshRotationCanceled: a rotation canceled while the
// replacement insert is in flight leaves the old token as it was.
func TestFault_Database_RefreshRotationCanceled(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer close(release)
		insert := faulttest.Sequence(faulttest.Block(release))
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{Statement: insert, Match: insertsRefreshToken})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := f.svc.RotateRefresh(ctx, f.refresh)
			done <- err
		}()
		<-insert.Reached(1) // revoked in the transaction, replacement in flight
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("RotateRefresh = %v, want context.Canceled", err)
		}
		f.assertUnrotated(t)
	})
}
