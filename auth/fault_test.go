package auth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

// selectsUsers matches a SELECT from the users table. Inside RotateRefresh
// the only one is the user-row lock; the dialect decides whether it carries
// FOR UPDATE (SQLite drops it), so this does not look for the text.
func selectsUsers(query string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "SELECT") && strings.Contains(query, "users")
}

// TestFault_Database_RotationUserLockFailure: the user-row lock a rotation
// takes first fails for an infrastructure reason (a deadlock, a lock
// timeout, a dropped connection). That is a server failure, not a dead
// credential: the caller must not see "invalid refresh token" (a 401 that
// signs the user out), and the token stays intact.
func TestFault_Database_RotationUserLockFailure(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailOnce(faulttest.ErrInjected),
			Match:     selectsUsers,
		})
		if _, err := f.svc.RotateRefresh(context.Background(), f.refresh); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("RotateRefresh = %v, want the injected lock failure", err)
		}
		f.assertUnrotated(t)
	})
}

// syncClock is an auth.Clock a test moves while the service reads it from
// another goroutine.
type syncClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *syncClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *syncClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// TestFault_Database_RotationReadsClockAfterUserLock: a rotation is held at
// the user-row lock it takes first (the wait behind another rotation or a
// revocation of the same user) while its refresh token expires. Expected
// outcome: the rotation reads the clock once it holds the lock, so it finds
// the token expired, fails as an invalid refresh token, and writes no
// successor. A rotation that read the clock before the wait would rotate a
// token that had expired, and date its successor from before the wait.
//
// The fault holds the SELECT on users (selectsUsers), not a FOR UPDATE, so it
// pins the same point on SQLite, whose dialect drops the locking clause, and
// the test runs on all three databases. What it pins is the clock read, not
// the server's lock, so the hook standing in for the wait is enough.
func TestFault_Database_RotationReadsClockAfterUserLock(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		lock := faulttest.Sequence(faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: lock, Match: selectsUsers}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		var token auth.RefreshToken
		if err := f.db.Where("user_id = ?", f.user.ID).First(&token).Error; err != nil {
			t.Fatal(err)
		}
		clock := &syncClock{now: token.ExpiresAt.Add(-time.Minute)}
		auth.SetClock(f.svc, clock)
		faults.Arm()

		rotateErr := make(chan error, 1)
		go func() {
			_, err := f.svc.RotateRefresh(ctx, f.refresh)
			rotateErr <- err
		}()
		wait(t, lock.Reached(1), "the rotation never reached the user's lock")
		clock.set(token.ExpiresAt.Add(time.Second)) // the token expires during the wait
		close(release)

		if err := waitErr(t, rotateErr, "rotation"); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("RotateRefresh = %v, want an invalid refresh token: the token expired while the rotation waited for the lock", err)
		}
		faulttest.Idle(t, f.db)
		var tokens []auth.RefreshToken
		if err := f.db.Where("user_id = ?", f.user.ID).Find(&tokens).Error; err != nil {
			t.Fatal(err)
		}
		if len(tokens) != 1 || tokens[0].RevokedAt != nil || tokens[0].ReplacedBy != nil {
			t.Fatalf("after the rotation of an expired token: %d tokens (first revoked: %v); want the one original, untouched",
				len(tokens), len(tokens) > 0 && tokens[0].RevokedAt != nil)
		}
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

// TestFault_Database_SessionRevocationCommitFailure: a session revocation
// whose commit fails reports the fault and revokes nothing, so the caller
// never believes a session ended when it did not.
func TestFault_Database_SessionRevocationCommitFailure(t *testing.T) {
	tests := []struct {
		name   string
		revoke func(ctx context.Context, f rotationFixture, current auth.TokenPair) error
	}{
		{
			name: "one",
			revoke: func(ctx context.Context, f rotationFixture, current auth.TokenPair) error {
				id, err := auth.SessionIDOf(ctx, f.svc, current.AccessToken)
				if err != nil {
					return err
				}
				return f.svc.RevokeSession(ctx, f.user.ID, id)
			},
		},
		{
			name: "others",
			revoke: func(ctx context.Context, f rotationFixture, current auth.TokenPair) error {
				row, err := auth.AuthenticatedRow(ctx, f.svc, current.AccessToken)
				if err != nil {
					return err
				}
				return auth.RevokeOtherSessionsKeeping(ctx, f.svc, f.user.ID, row)
			},
		},
		{
			name: "all",
			revoke: func(ctx context.Context, f rotationFixture, _ auth.TokenPair) error {
				return f.svc.RevokeAllSessions(ctx, f.user.ID)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
				faults := &faulttest.DBFaults{Commit: faulttest.FailOnce(faulttest.ErrInjected)}
				f := newRotationFixture(t, kind, dsn, faults)
				ctx := context.Background()
				faults.Disarm()
				current, err := f.svc.IssueTokens(ctx, f.user)
				if err != nil {
					t.Fatal(err)
				}
				faults.Arm()
				if err := tt.revoke(ctx, f, current); !errors.Is(err, faulttest.ErrInjected) {
					t.Fatalf("revoke = %v, want the commit fault", err)
				}
				faulttest.Idle(t, f.db)
				sessions, err := f.svc.ListSessions(ctx, f.user.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(sessions) != 2 {
					t.Fatalf("after a failed revocation: %d active sessions, want both", len(sessions))
				}
				if _, err := f.svc.ParseAccess(ctx, current.AccessToken); err != nil {
					t.Fatalf("current session after a failed revocation: %v", err)
				}
			})
		})
	}
}

// userSelects is a DBFaults.Match that records the SELECTs on users a faulted
// database sees, so a test can name which of them its injector hit.
type userSelects struct {
	mu      sync.Mutex
	queries []string
}

func (u *userSelects) match(query string) bool {
	if !selectsUsers(query) {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.queries = append(u.queries, query)
	return true
}

func (u *userSelects) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.queries = nil
}

func (u *userSelects) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.queries...)
}

// TestFault_Database_SessionRevocationLockFailure: the user-row lock a
// session revocation takes first fails for an infrastructure reason. The
// caller gets that failure, not ErrSessionNotFound (a 404 would tell the
// client the session is gone, and it is not), and no session is revoked.
//
// The injector hits the SELECT on users and nothing else. The test works out
// the session's row id and id before arming (reading them reads users too,
// without a lock, and on SQLite so would the lock: the dialect drops FOR
// UPDATE) and revokes "others" by row id (RevokeOtherSessionsKeeping), so the
// revocation's one SELECT on users is the lock; the test checks that, and on
// PostgreSQL and MySQL that it carries FOR [NO KEY] UPDATE.
func TestFault_Database_SessionRevocationLockFailure(t *testing.T) {
	tests := []struct {
		name   string
		revoke func(ctx context.Context, f rotationFixture, sessionID string, row uint) error
	}{
		{
			name: "one",
			revoke: func(ctx context.Context, f rotationFixture, sessionID string, _ uint) error {
				return f.svc.RevokeSession(ctx, f.user.ID, sessionID)
			},
		},
		{
			name: "others",
			revoke: func(ctx context.Context, f rotationFixture, _ string, row uint) error {
				return auth.RevokeOtherSessionsKeeping(ctx, f.svc, f.user.ID, row)
			},
		},
		{
			name: "all",
			revoke: func(ctx context.Context, f rotationFixture, _ string, _ uint) error {
				return f.svc.RevokeAllSessions(ctx, f.user.ID)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
				seen := &userSelects{}
				lock := faulttest.FailOnce(faulttest.ErrInjected)
				faults := &faulttest.DBFaults{Statement: lock, Match: seen.match}
				f := newRotationFixture(t, kind, dsn, faults)
				ctx := context.Background()
				faults.Disarm()
				current, err := f.svc.IssueTokens(ctx, f.user)
				if err != nil {
					t.Fatal(err)
				}
				row, err := auth.AuthenticatedRow(ctx, f.svc, current.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				sessionID, err := auth.SessionIDOf(ctx, f.svc, current.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				seen.reset()
				faults.Arm()

				err = tt.revoke(ctx, f, sessionID, row)
				if !errors.Is(err, faulttest.ErrInjected) {
					t.Fatalf("revoke = %v, want the injected lock failure", err)
				}
				if errors.Is(err, auth.ErrSessionNotFound) {
					t.Fatalf("revoke = %v: a failed lock was reported as a missing session", err)
				}
				queries := seen.seen()
				if lock.Calls() != 1 || len(queries) != 1 {
					t.Fatalf("%d SELECTs on users after arming (%q), want only the lock", len(queries), queries)
				}
				if kind != database.DriverSQLite && !locksUserRow(queries[0]) {
					t.Fatalf("the SELECT on users the fault hit is not the row lock: %s", queries[0])
				}
				faulttest.Idle(t, f.db)
				sessions, err := f.svc.ListSessions(ctx, f.user.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(sessions) != 2 {
					t.Fatalf("after a failed revocation: %d active sessions, want both", len(sessions))
				}
				if _, err := f.svc.ParseAccess(ctx, current.AccessToken); err != nil {
					t.Fatalf("current session after a failed revocation: %v", err)
				}
			})
		})
	}
}

// TestFault_HTTP_SessionRevocationLockFailure is the same failure through the
// routes: DELETE /auth/sessions?scope=all, ?scope=others and
// /auth/sessions/{id} answer 500 with the internal error envelope, not 404,
// and end nothing.
//
// The bearer middleware reads users before the handler does (parseAccess, no
// lock), so the fault is on the second SELECT on users, and the test checks
// that is the handler's lock: on PostgreSQL and MySQL it carries FOR [NO KEY]
// UPDATE, and the first does not.
func TestFault_HTTP_SessionRevocationLockFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const sessions = "/api/v1/auth/sessions"
	tests := []struct {
		name string
		path func(otherID string) string
	}{
		{"scope all", func(string) string { return sessions + "?scope=all" }},
		{"scope others", func(string) string { return sessions + "?scope=others" }},
		{"one", func(otherID string) string { return sessions + "/" + otherID }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
				seen := &userSelects{}
				lock := faulttest.FailOnCall(2, faulttest.ErrInjected)
				faults := &faulttest.DBFaults{Statement: lock, Match: seen.match}
				faults.Disarm()
				db, err := faulttest.OpenDB(kind, dsn, faults)
				if err != nil {
					t.Fatal(err)
				}
				dropAuthTables := func() {
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
				app := newAuthAppWithDB(t, db)
				registerUser(t, app, "locked@example.com", testPassword)
				laptop := loginUser(t, app, "locked@example.com", testPassword)
				loginUser(t, app, "locked@example.com", testPassword)
				_, others := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, laptop.AccessToken)))
				seen.reset()
				faults.Arm()

				rec := bearerRequest(app, http.MethodDelete, tt.path(others[0]), laptop.AccessToken)
				assertError(t, rec, http.StatusInternalServerError, "internal")
				queries := seen.seen()
				if lock.Calls() != 2 || len(queries) != 2 {
					t.Fatalf("%d SELECTs on users after arming (%q), want the middleware's and the lock", len(queries), queries)
				}
				if kind != database.DriverSQLite && (locksUserRow(queries[0]) || !locksUserRow(queries[1])) {
					t.Fatalf("the fault must hit the handler's row lock, the second SELECT on users:\n1: %s\n2: %s", queries[0], queries[1])
				}
				faulttest.Idle(t, db)
				if listed := decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, laptop.AccessToken)); len(listed) != 2 {
					t.Fatalf("after the failed revocation: %d sessions, want both", len(listed))
				}
			})
		})
	}
}
