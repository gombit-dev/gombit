package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// TestFault_Concurrency_RotationLeaderFails: several callers rotate the same
// refresh token at once while the rotating transaction stalls and then
// fails (the replacement token's insert). auth.Service shares one rotation
// among concurrent callers of the same token: the followers are pinned
// inside that shared rotation (joined, waiting on the leader) before the
// leader is released. Expected outcome: a terminal error for everyone and no
// corruption. Every caller returns (none is wedged behind the failed
// leader), every caller sees the leader's failure, only the leader touched
// the database (one INSERT: nobody ran an independent rotation), the token is
// not revoked or replaced, and once the fault clears it rotates.
func TestFault_Concurrency_RotationLeaderFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		insert := faulttest.SequenceThen(faulttest.Failure(faulttest.ErrInjected),
			faulttest.BlockThenFail(release, faulttest.ErrInjected))
		f := newRotationFixture(t, kind, dsn, &faulttest.DBFaults{Statement: insert, Match: insertsRefreshToken})

		const followers = 4
		joined := make(chan struct{}, followers)
		auth.SetRotateJoinHook(f.svc, func() { joined <- struct{}{} })
		results := make(chan error, followers+1)
		rotate := func() {
			_, err := f.svc.RotateRefresh(context.Background(), f.refresh)
			results <- err
		}
		go rotate()
		<-insert.Reached(1) // the leader is inside its transaction, stalled
		for i := 0; i < followers; i++ {
			go rotate()
		}
		for i := 0; i < followers; i++ {
			select {
			case <-joined: // a follower is waiting on the leader's rotation
			case err := <-results:
				t.Fatalf("a follower returned (%v) without joining the stalled leader's rotation", err)
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d followers never joined the in-flight rotation", followers-i, followers)
			}
		}
		close(release) // the leader's insert fails; its transaction rolls back

		for i := 0; i < followers+1; i++ {
			select {
			case err := <-results:
				if !errors.Is(err, faulttest.ErrInjected) {
					t.Fatalf("a caller got %v, want the rotation's failure", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%d of %d callers are wedged behind the failed rotation", followers+1-i, followers+1)
			}
		}
		if n := insert.Calls(); n != 1 {
			t.Fatalf("%d replacement-token inserts, want 1: every follower should have shared the leader's rotation", n)
		}
		insert.Disarm() // the fault clears
		f.assertUnrotated(t)
	})
}

// TestFault_Concurrency_RevocationDuringRotation: a session is rotating (its
// transaction is pinned at the replacement token's insert) when the user
// revokes it along with the others. Expected outcome: the revocation
// covers the rotation's result — the replacement token the rotation commits
// does not authenticate and cannot refresh — while the session that asked
// for the revocation survives "others". Revocation and rotation serialize
// on the user's row; without that, PostgreSQL's revocation would wait on the
// rotating row, re-check only it, and never see the replacement. The
// rotation is released only once the revocation has issued its user lock,
// so a revocation path that skips the lock fails the test deterministically
// instead of racing the rotation's commit.
//
// SQLite is skipped: its pool has one connection, so the revocation cannot
// start while the rotation's transaction holds it.
func TestFault_Concurrency_RevocationDuringRotation(t *testing.T) {
	dbs := nonSQLite()
	tests := []struct {
		name            string
		revoke          func(ctx context.Context, f rotationFixture, current auth.TokenPair) error
		currentSurvives bool
	}{
		{
			name: "others",
			revoke: func(ctx context.Context, f rotationFixture, current auth.TokenPair) error {
				row, err := auth.AuthenticatedRow(ctx, f.svc, current.AccessToken)
				if err != nil {
					return err
				}
				return auth.RevokeOtherSessionsKeeping(ctx, f.svc, f.user.ID, row)
			},
			currentSurvives: true,
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
			if len(dbs) == 0 {
				t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
			}
			faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
				release := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				// Statement call 1 is the rotation's user lock, call 2 its
				// replacement insert (held), call 3 the revocation's user lock.
				stmts := faulttest.Sequence(faulttest.Success(), faulttest.Block(release))
				faults := &faulttest.DBFaults{Statement: stmts, Match: locksUserOrInsertsRefreshToken}
				f := newRotationFixture(t, kind, dsn, faults)
				ctx := context.Background()
				faults.Disarm()
				current, err := f.svc.IssueTokens(ctx, f.user)
				if err != nil {
					t.Fatal(err)
				}
				faults.Arm()

				rotated := make(chan auth.TokenPair, 1)
				rotateErr := make(chan error, 1)
				go func() {
					pair, err := f.svc.RotateRefresh(ctx, f.refresh)
					rotated <- pair
					rotateErr <- err
				}()
				wait(t, stmts.Reached(2), "the rotation never reached its insert")

				revokeErr := make(chan error, 1)
				go func() { revokeErr <- tt.revoke(ctx, f, current) }()
				wait(t, stmts.Reached(3), "the revocation never took the user's lock")
				// Reaching the statement hook says nothing about the server:
				// release the rotation only once the revocation is really
				// waiting for the user's row. A lock the revocation did not
				// hold in its transaction would be dropped at once and
				// never wait.
				faulttest.AwaitLockWait(t, kind, dsn, "users")
				close(release) // the rotation commits its replacement token

				if err := waitErr(t, rotateErr, "rotation"); err != nil {
					t.Fatalf("RotateRefresh = %v; it held the user's lock first and should commit", err)
				}
				replacement := <-rotated
				if err := waitErr(t, revokeErr, "revocation"); err != nil {
					t.Fatalf("revoke = %v", err)
				}
				if _, err := f.svc.ParseAccess(ctx, replacement.AccessToken); err == nil {
					t.Fatal("the session rotated during the revocation still authenticates")
				}
				if _, err := f.svc.RotateRefresh(ctx, replacement.RefreshToken); err == nil {
					t.Fatal("the session rotated during the revocation can still refresh")
				}
				if _, err := f.svc.ParseAccess(ctx, current.AccessToken); (err == nil) != tt.currentSurvives {
					t.Fatalf("current session authenticates = %v, want %v", err == nil, tt.currentSurvives)
				}
			})
		})
	}
}

// TestFault_Concurrency_RevokeOneDuringRotation: session S is rotating (its
// transaction is pinned at the replacement token's insert, holding the
// user's lock) when the user revokes S by the id it was listed with before
// that rotation. Expected outcome: the revocation waits for the user's row,
// then finds that the id no longer names an active session and returns
// ErrSessionNotFound (a 404: list again) without revoking anything, and S's
// new head authenticates and rotates. A revocation that read the sessions
// before taking the lock would match S's old head, wait, and then update a
// row the rotation had already revoked: it would report success while S
// lives on. The rotation is released only once the revocation is really
// waiting for the user's row, so that ordering fails the test
// deterministically rather than racing the rotation's commit.
//
// SQLite is skipped like the tests above.
func TestFault_Concurrency_RevokeOneDuringRotation(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		// Statement call 1 is S's user lock, call 2 S's replacement insert
		// (held), call 3 the revocation's user lock.
		stmts := faulttest.Sequence(faulttest.Success(), faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: stmts, Match: locksUserOrInsertsRefreshToken}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		listed, err := f.svc.ListSessions(ctx, f.user.ID)
		if err != nil || len(listed) != 1 {
			t.Fatalf("ListSessions = %v, %v; want the fixture's one session", listed, err)
		}
		faults.Arm()

		rotated := make(chan auth.TokenPair, 1)
		rotateErr := make(chan error, 1)
		go func() {
			pair, err := f.svc.RotateRefresh(ctx, f.refresh)
			rotated <- pair
			rotateErr <- err
		}()
		wait(t, stmts.Reached(2), "the rotation never reached its insert")

		revokeErr := make(chan error, 1)
		go func() { revokeErr <- f.svc.RevokeSession(ctx, f.user.ID, listed[0].ID) }()
		wait(t, stmts.Reached(3), "the revocation never took the user's lock")
		faulttest.AwaitLockWait(t, kind, dsn, "users")
		close(release) // the rotation commits S's new head

		if err := waitErr(t, rotateErr, "rotation"); err != nil {
			t.Fatalf("RotateRefresh = %v; it held the user's lock first and should commit", err)
		}
		head := <-rotated
		if err := waitErr(t, revokeErr, "revocation"); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("RevokeSession(the id S was listed with before it rotated) = %v, want ErrSessionNotFound", err)
		}
		if _, err := f.svc.ParseAccess(ctx, head.AccessToken); err != nil {
			t.Fatalf("S's new head does not authenticate (%v); the revocation of its stale id must end nothing", err)
		}
		if _, err := f.svc.RotateRefresh(ctx, head.RefreshToken); err != nil {
			t.Fatalf("S's new head cannot refresh (%v); the revocation of its stale id must end nothing", err)
		}
	})
}

// TestFault_Concurrency_RevokeOneExpiresWhileWaiting: a revocation of one
// session S waits for the user's row, which a rotation of the user's other
// session holds (pinned at its replacement insert), and S expires during that
// wait. Expected outcome: the revocation reads the active sessions once it
// holds the lock, finds S expired, and returns ErrSessionNotFound without
// writing: S's revoked_at stays NULL. A revocation that read the sessions
// before taking the lock would match S while it was still active, wait, and
// then end a session that had already expired and report success. The
// rotation is released only once the revocation is really waiting for the
// user's row, and the clock moves past S's expiry only then.
//
// SQLite is skipped like the tests above.
func TestFault_Concurrency_RevokeOneExpiresWhileWaiting(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		// Statement call 1 is the other session's user lock, call 2 its
		// replacement insert (held), call 3 the revocation's user lock.
		stmts := faulttest.Sequence(faulttest.Success(), faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: stmts, Match: locksUserOrInsertsRefreshToken}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		// f.refresh is the other session. S signs in an hour before it, so S
		// expires an hour before it does.
		var other auth.RefreshToken
		if err := f.db.Where("user_id = ?", f.user.ID).First(&other).Error; err != nil {
			t.Fatal(err)
		}
		clock := &syncClock{now: other.CreatedAt.Add(-time.Hour)}
		auth.SetClock(f.svc, clock)
		s, err := f.svc.IssueTokens(ctx, f.user)
		if err != nil {
			t.Fatal(err)
		}
		sID, err := auth.SessionIDOf(ctx, f.svc, s.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		sRowID, err := auth.AuthenticatedRow(ctx, f.svc, s.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		var sRow auth.RefreshToken
		if err := f.db.First(&sRow, sRowID).Error; err != nil {
			t.Fatal(err)
		}
		clock.set(sRow.ExpiresAt.Add(-time.Minute)) // both sessions are active
		faults.Arm()

		rotateErr := make(chan error, 1)
		go func() {
			_, err := f.svc.RotateRefresh(ctx, f.refresh)
			rotateErr <- err
		}()
		wait(t, stmts.Reached(2), "the other session's rotation never reached its insert")

		revokeErr := make(chan error, 1)
		go func() { revokeErr <- f.svc.RevokeSession(ctx, f.user.ID, sID) }()
		wait(t, stmts.Reached(3), "the revocation never took the user's lock")
		faulttest.AwaitLockWait(t, kind, dsn, "users")
		clock.set(sRow.ExpiresAt.Add(time.Second)) // S expires during the wait
		close(release)                             // the rotation commits

		if err := waitErr(t, rotateErr, "rotation"); err != nil {
			t.Fatalf("RotateRefresh = %v; it held the user's lock first and should commit", err)
		}
		if err := waitErr(t, revokeErr, "revocation"); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("RevokeSession(a session that expired while it waited for the user's lock) = %v, want ErrSessionNotFound", err)
		}
		faulttest.Idle(t, f.db)
		if err := f.db.First(&sRow, sRow.ID).Error; err != nil {
			t.Fatal(err)
		}
		if sRow.RevokedAt != nil {
			t.Fatal("the revocation revoked a session that had expired before it held the user's lock")
		}
	})
}

// TestFault_Concurrency_LogoutDuringRevokeOne: a revocation of one session
// has found the session's row and is pinned just before its UPDATE (holding
// the user's lock) when the same session logs out. Logout takes no user lock,
// so it runs to the end. Expected outcome: once released, the revocation
// updates nothing and returns ErrSessionNotFound, never a success for a
// session it did not end; the session stays ended. The 404 must come from
// the revocation's own update, not from when the database fixed its view of
// the rows.
//
// SQLite is skipped like the tests above.
func TestFault_Concurrency_LogoutDuringRevokeOne(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		// Statement call 1 is the revocation's UPDATE of the tokens (held),
		// call 2 the logout's.
		update := faulttest.Sequence(faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: update, Match: updatesRefreshTokens}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		listed, err := f.svc.ListSessions(ctx, f.user.ID)
		if err != nil || len(listed) != 1 {
			t.Fatalf("ListSessions = %v, %v; want the fixture's one session", listed, err)
		}
		faults.Arm()

		revokeErr := make(chan error, 1)
		go func() { revokeErr <- f.svc.RevokeSession(ctx, f.user.ID, listed[0].ID) }()
		wait(t, update.Reached(1), "the revocation never reached its update")

		logoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := f.svc.RevokeRefresh(logoutCtx, f.refresh); err != nil {
			t.Fatalf("logout during the revocation = %v; it takes no user lock and must not wait for it", err)
		}
		close(release) // the revocation updates the row the logout revoked

		if err := waitErr(t, revokeErr, "revocation"); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("RevokeSession of a session logged out under it = %v, want ErrSessionNotFound", err)
		}
		if _, err := f.svc.RotateRefresh(ctx, f.refresh); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("RotateRefresh after logout = %v, want an invalid refresh token", err)
		}
	})
}

// TestFault_Concurrency_RevokeOthersDuringOwnRotation: the session that asks
// to revoke the others is itself rotating (its transaction is pinned at the
// replacement token's insert, holding the user's lock) when the revocation
// starts, keyed by the row the request was authenticated as before that
// rotation. Expected outcome: the revocation waits for the user's row, then
// follows replaced_by from that row to the requesting session's new head and
// keeps it: the new head authenticates and rotates, while the user's other
// session is ended. A revocation that walked the chain before taking the
// lock would find no successor yet and then revoke the new head the rotation
// committed: asking to sign out the others would sign out the asker too.
//
// SQLite is skipped like the tests above.
func TestFault_Concurrency_RevokeOthersDuringOwnRotation(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		// Statement call 1 is the requester's user lock, call 2 its
		// replacement insert (held), call 3 the revocation's user lock.
		stmts := faulttest.Sequence(faulttest.Success(), faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: stmts, Match: locksUserOrInsertsRefreshToken}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		// f.refresh is the requesting session; its only row so far is the one
		// its access token is bound to, what the middleware recorded.
		var authenticated auth.RefreshToken
		if err := f.db.Where("user_id = ?", f.user.ID).First(&authenticated).Error; err != nil {
			t.Fatal(err)
		}
		other, err := f.svc.IssueTokens(ctx, f.user)
		if err != nil {
			t.Fatal(err)
		}
		faults.Arm()

		rotated := make(chan auth.TokenPair, 1)
		rotateErr := make(chan error, 1)
		go func() {
			pair, err := f.svc.RotateRefresh(ctx, f.refresh)
			rotated <- pair
			rotateErr <- err
		}()
		wait(t, stmts.Reached(2), "the requester's rotation never reached its insert")

		revokeErr := make(chan error, 1)
		go func() {
			revokeErr <- auth.RevokeOtherSessionsKeeping(ctx, f.svc, f.user.ID, authenticated.ID)
		}()
		wait(t, stmts.Reached(3), "the revocation never took the user's lock")
		faulttest.AwaitLockWait(t, kind, dsn, "users")
		close(release) // the rotation commits the requester's new head

		if err := waitErr(t, rotateErr, "rotation"); err != nil {
			t.Fatalf("RotateRefresh = %v; it held the user's lock first and should commit", err)
		}
		head := <-rotated
		if err := waitErr(t, revokeErr, "revocation"); err != nil {
			t.Fatalf("revoke others = %v", err)
		}
		if _, err := f.svc.ParseAccess(ctx, head.AccessToken); err != nil {
			t.Fatalf("the requester's new head does not authenticate (%v); revoking the others ended the requester", err)
		}
		if _, err := f.svc.RotateRefresh(ctx, head.RefreshToken); err != nil {
			t.Fatalf("the requester's new head cannot refresh (%v); revoking the others ended the requester", err)
		}
		if _, err := f.svc.ParseAccess(ctx, other.AccessToken); err == nil {
			t.Fatal("the other session still authenticates after the others were revoked")
		}
	})
}

// TestFault_Concurrency_ListCurrentDuringOwnRotation: a request that lists
// the sessions is held after the middleware checked its access token, just
// before it reads them, while the session it belongs to refreshes twice (its
// row A is replaced by B, then B by C); the user has another session too.
// Expected outcome: the list marks exactly one session current, the
// requesting session's head C, and the other session not. The middleware
// recorded A, which no longer is an active row by the time the list is read:
// a list that marked only that row would mark none. It runs in both auth
// modes.
//
// SQLite is skipped like the tests above: the held read keeps its only
// connection, which the rotations need.
func TestFault_Concurrency_ListCurrentDuringOwnRotation(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	for _, mode := range sessionsRequestModes {
		t.Run(mode.name, func(t *testing.T) {
			faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
				release := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				// Statement call 1 is the request's read of the active
				// sessions (held).
				list := faulttest.Sequence(faulttest.Block(release))
				faults := &faulttest.DBFaults{Statement: list, Match: readsActiveSessions}
				f := newRotationFixture(t, kind, dsn, faults)
				ctx := context.Background()
				faults.Disarm()
				app := mode.app(t, f.db)
				// f.refresh is the other session; a is the requesting one.
				a, err := f.svc.IssueTokens(ctx, f.user)
				if err != nil {
					t.Fatal(err)
				}
				aID, err := auth.SessionIDOf(ctx, f.svc, a.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				before, err := f.svc.ListSessions(ctx, f.user.ID)
				if err != nil || len(before) != 2 {
					t.Fatalf("ListSessions = %v, %v; want the two sessions", before, err)
				}
				otherID := before[0].ID
				if otherID == aID {
					otherID = before[1].ID
				}
				faults.Arm()

				listed := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil)
					mode.send(req, a.AccessToken)
					app.Router().ServeHTTP(rec, req)
					listed <- rec
				}()
				wait(t, list.Reached(1), "the request never reached its read of the sessions")
				b, err := f.svc.RotateRefresh(ctx, a.RefreshToken)
				if err != nil {
					t.Fatalf("RotateRefresh(A) = %v", err)
				}
				c, err := f.svc.RotateRefresh(ctx, b.RefreshToken)
				if err != nil {
					t.Fatalf("RotateRefresh(B) = %v", err)
				}
				close(release) // the request reads the sessions: C and the other

				var rec *httptest.ResponseRecorder
				select {
				case rec = <-listed:
				case <-time.After(5 * time.Second):
					t.Fatal("the request is wedged")
				}
				headID, err := auth.SessionIDOf(ctx, f.svc, c.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				current, others := splitCurrent(t, decodeSessions(t, rec))
				if current != headID || len(others) != 1 || others[0] != otherID {
					t.Fatalf("listed current %q and others %v; want C %q current and only the other session %q not",
						current, others, headID, otherID)
				}
			})
		})
	}
}

// TestFault_Concurrency_ListCurrentWhenRotatedAfterListing: a request that
// lists the sessions reads them after its session refreshed once (its row A
// replaced by B) and is held again before it follows that session's chain
// from A, while its session refreshes again (B replaced by C) and the user's
// other session refreshes too. Expected outcome: the list, read before those
// two refreshes, marks exactly one session current, B, the row of the
// requesting session it read, and the other session, read under its old row,
// not. The chain followed after the list was read runs on past B to C: a list
// that marked the chain's last row would mark a row it never read, so none.
// It runs in both auth modes.
//
// SQLite is skipped like the tests above.
func TestFault_Concurrency_ListCurrentWhenRotatedAfterListing(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	for _, mode := range sessionsRequestModes {
		t.Run(mode.name, func(t *testing.T) {
			faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
				readRelease, chainRelease := make(chan struct{}), make(chan struct{})
				defer func() {
					for _, release := range []chan struct{}{readRelease, chainRelease} {
						select {
						case <-release:
						default:
							close(release)
						}
					}
				}()
				// Statement call 1 is the request's read of the active
				// sessions (held), call 2 the first step of its walk along
				// its session's chain (held).
				stmts := faulttest.Sequence(faulttest.Block(readRelease), faulttest.Block(chainRelease))
				faults := &faulttest.DBFaults{Statement: stmts, Match: readsActiveSessionsOrChainRow}
				f := newRotationFixture(t, kind, dsn, faults)
				ctx := context.Background()
				faults.Disarm()
				app := mode.app(t, f.db)
				// f.refresh is the other session; a is the requesting one.
				a, err := f.svc.IssueTokens(ctx, f.user)
				if err != nil {
					t.Fatal(err)
				}
				aID, err := auth.SessionIDOf(ctx, f.svc, a.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				before, err := f.svc.ListSessions(ctx, f.user.ID)
				if err != nil || len(before) != 2 {
					t.Fatalf("ListSessions = %v, %v; want the two sessions", before, err)
				}
				otherID := before[0].ID
				if otherID == aID {
					otherID = before[1].ID
				}
				faults.Arm()

				listed := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil)
					mode.send(req, a.AccessToken)
					app.Router().ServeHTTP(rec, req)
					listed <- rec
				}()
				wait(t, stmts.Reached(1), "the request never reached its read of the sessions")
				b, err := f.svc.RotateRefresh(ctx, a.RefreshToken)
				if err != nil {
					t.Fatalf("RotateRefresh(A) = %v", err)
				}
				// B's id, while B still authenticates.
				listedID, err := auth.SessionIDOf(ctx, f.svc, b.AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				close(readRelease) // the request reads the sessions: B and the other
				wait(t, stmts.Reached(2), "the request never reached its walk along the chain")
				if _, err := f.svc.RotateRefresh(ctx, b.RefreshToken); err != nil {
					t.Fatalf("RotateRefresh(B) = %v", err)
				}
				if _, err := f.svc.RotateRefresh(ctx, f.refresh); err != nil {
					t.Fatalf("RotateRefresh(other) = %v", err)
				}
				close(chainRelease) // the walk runs A, B, C

				var rec *httptest.ResponseRecorder
				select {
				case rec = <-listed:
				case <-time.After(5 * time.Second):
					t.Fatal("the request is wedged")
				}
				current, others := splitCurrent(t, decodeSessions(t, rec))
				if current != listedID || len(others) != 1 || others[0] != otherID {
					t.Fatalf("listed current %q and others %v; want B %q current and only the other session %q not",
						current, others, listedID, otherID)
				}
			})
		})
	}
}

// sessionsRequestModes send a sessions request's access token the way each
// auth mode does: a Bearer header, or the access cookie.
var sessionsRequestModes = []struct {
	name string
	app  func(*testing.T, *database.DB) *framework.App
	send func(req *http.Request, access string)
}{
	{"bearer", newAuthAppWithDB, func(req *http.Request, access string) {
		req.Header.Set("Authorization", "Bearer "+access)
	}},
	{"cookie", newCookieAuthAppWithDB, func(req *http.Request, access string) {
		req.AddCookie(&http.Cookie{Name: auth.AccessCookieName, Value: access}) //nolint:gosec // G124: request Cookie header only carries name/value.
	}},
}

// TestFault_Concurrency_RotationDuringRevocation is the same race the other
// way round: a revocation holds the user's lock, pinned just before it
// updates the tokens, when the session it ends starts to rotate. Expected
// outcome: the rotation waits for the user's row on the server and does
// nothing else (no compare-and-swap, no replacement insert) until the
// revocation commits; it then finds its token revoked and fails as an
// invalid refresh token, and no replacement exists. This pins the lock order,
// user before token, which both paths must keep: a rotation that took the
// token first would deadlock against the revocation, and one that did not
// wait would insert a replacement behind it. A revocation that held the lock
// outside its transaction (released at once) would never make the rotation
// wait.
//
// SQLite is skipped like the test above.
func TestFault_Concurrency_RotationDuringRevocation(t *testing.T) {
	dbs := nonSQLite()
	tests := []struct {
		name            string
		revoke          func(ctx context.Context, f rotationFixture, current auth.TokenPair, rotatingSession string) error
		currentSurvives bool
	}{
		{
			name: "one",
			revoke: func(ctx context.Context, f rotationFixture, _ auth.TokenPair, rotatingSession string) error {
				return f.svc.RevokeSession(ctx, f.user.ID, rotatingSession)
			},
			currentSurvives: true,
		},
		{
			name: "others",
			revoke: func(ctx context.Context, f rotationFixture, current auth.TokenPair, _ string) error {
				row, err := auth.AuthenticatedRow(ctx, f.svc, current.AccessToken)
				if err != nil {
					return err
				}
				return auth.RevokeOtherSessionsKeeping(ctx, f.svc, f.user.ID, row)
			},
			currentSurvives: true,
		},
		{
			name: "all",
			revoke: func(ctx context.Context, f rotationFixture, _ auth.TokenPair, _ string) error {
				return f.svc.RevokeAllSessions(ctx, f.user.ID)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(dbs) == 0 {
				t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
			}
			faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
				release := make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				// Matched statement 1 is the revocation's user lock, 2 its
				// UPDATE of the tokens (held), 3 the rotation's user lock. A
				// 4th would be the rotation going on without the lock.
				stmts := faulttest.Sequence(faulttest.Success(), faulttest.Block(release))
				faults := &faulttest.DBFaults{Statement: stmts, Match: locksUserOrWritesRefreshTokens}
				f := newRotationFixture(t, kind, dsn, faults)
				ctx := context.Background()
				faults.Disarm()
				listed, err := f.svc.ListSessions(ctx, f.user.ID)
				if err != nil || len(listed) != 1 {
					t.Fatalf("ListSessions = %v, %v; want the fixture's one session", listed, err)
				}
				rotatingSession := listed[0].ID
				current, err := f.svc.IssueTokens(ctx, f.user)
				if err != nil {
					t.Fatal(err)
				}
				faults.Arm()

				revokeErr := make(chan error, 1)
				go func() { revokeErr <- tt.revoke(ctx, f, current, rotatingSession) }()
				wait(t, stmts.Reached(2), "the revocation never reached its update")

				rotateErr := make(chan error, 1)
				go func() {
					_, err := f.svc.RotateRefresh(ctx, f.refresh)
					rotateErr <- err
				}()
				wait(t, stmts.Reached(3), "the rotation never took the user's lock")
				faulttest.AwaitLockWait(t, kind, dsn, "users")
				if n := stmts.Calls(); n != 3 {
					t.Fatalf("%d statements on the user lock or the tokens, want 3: the rotation went past the user's lock the revocation holds", n)
				}
				select {
				case err := <-rotateErr:
					t.Fatalf("RotateRefresh returned (%v) while the revocation held the user's lock", err)
				default:
				}
				close(release) // the revocation updates the tokens and commits

				if err := waitErr(t, revokeErr, "revocation"); err != nil {
					t.Fatalf("revoke = %v; it held the user's lock first and should commit", err)
				}
				if err := waitErr(t, rotateErr, "rotation"); !errors.Is(err, auth.ErrInvalidRefreshToken) {
					t.Fatalf("RotateRefresh = %v, want an invalid refresh token: its session was revoked first", err)
				}
				faulttest.Idle(t, f.db)
				var tokens []auth.RefreshToken
				if err := f.db.Where("user_id = ?", f.user.ID).Find(&tokens).Error; err != nil {
					t.Fatal(err)
				}
				if len(tokens) != 2 {
					t.Fatalf("%d refresh tokens, want 2: the rotation left a replacement behind the revocation", len(tokens))
				}
				for _, token := range tokens {
					if token.ReplacedBy != nil {
						t.Fatalf("token %d was replaced by %d; the rotation must not have run", token.ID, *token.ReplacedBy)
					}
				}
				if _, err := f.svc.ParseAccess(ctx, current.AccessToken); (err == nil) != tt.currentSurvives {
					t.Fatalf("current session authenticates = %v, want %v", err == nil, tt.currentSurvives)
				}
			})
		})
	}
}

// TestFault_Concurrency_ReuseCascadeDuringRotation: session B is rotating
// (its transaction is pinned at the replacement token's insert, holding the
// user's lock) when a token of session A that was already rotated is
// presented again. Expected outcome: the reuse cascade waits for the user's
// row, then ends every session, B's replacement included: RotateRefresh
// returns ErrRefreshReuse, and neither B's new tokens nor A's authenticate or
// refresh. The cascade runs inside the rotation transaction, so it takes the
// same lock; one that took it after the reuse branch (or not at all) would
// revoke only the rows it saw before B's replacement committed, and B's
// session would survive (on PostgreSQL, where the UPDATE re-checks only the
// rows it waited on). The test releases B only once the cascade is really
// waiting for the user's row, so that ordering fails it deterministically
// rather than racing B's commit.
//
// SQLite is skipped like the tests above.
func TestFault_Concurrency_ReuseCascadeDuringRotation(t *testing.T) {
	dbs := nonSQLite()
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL or MySQL (-auth.postgres-dsn / -auth.mysql-dsn)")
	}
	faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		// Statement call 1 is B's user lock, call 2 B's replacement insert
		// (held), call 3 the user lock of the rotation that presents A's
		// rotated token.
		stmts := faulttest.Sequence(faulttest.Success(), faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: stmts, Match: locksUserOrInsertsRefreshToken}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		// f.refresh is session B's token. Session A is rotated once (A -> A2),
		// so A's first token is a reuse when it comes back.
		a, err := f.svc.IssueTokens(ctx, f.user)
		if err != nil {
			t.Fatal(err)
		}
		a2, err := f.svc.RotateRefresh(ctx, a.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		faults.Arm()

		rotated := make(chan auth.TokenPair, 1)
		rotateErr := make(chan error, 1)
		go func() {
			pair, err := f.svc.RotateRefresh(ctx, f.refresh)
			rotated <- pair
			rotateErr <- err
		}()
		wait(t, stmts.Reached(2), "B's rotation never reached its insert")

		reuseErr := make(chan error, 1)
		go func() {
			_, err := f.svc.RotateRefresh(ctx, a.RefreshToken)
			reuseErr <- err
		}()
		wait(t, stmts.Reached(3), "the reuse never took the user's lock")
		// Reaching the statement hook says nothing about the server: release
		// B only once the reuse is really waiting for the user's row.
		faulttest.AwaitLockWait(t, kind, dsn, "users")
		close(release) // B commits its replacement token

		if err := waitErr(t, rotateErr, "B's rotation"); err != nil {
			t.Fatalf("B's RotateRefresh = %v; it held the user's lock first and should commit", err)
		}
		b2 := <-rotated
		if err := waitErr(t, reuseErr, "reuse"); !errors.Is(err, auth.ErrRefreshReuse) {
			t.Fatalf("RotateRefresh(A's rotated token) = %v, want ErrRefreshReuse", err)
		}
		for name, pair := range map[string]auth.TokenPair{"B's new session": b2, "A's session": a2} {
			if _, err := f.svc.ParseAccess(ctx, pair.AccessToken); err == nil {
				t.Errorf("%s still authenticates after the reuse cascade", name)
			}
			if _, err := f.svc.RotateRefresh(ctx, pair.RefreshToken); err == nil {
				t.Errorf("%s can still refresh after the reuse cascade", name)
			}
		}
	})
}

// TestFault_Concurrency_RotationLockAllowsForeignKeyInserts: while a rotation
// holds the user's lock (pinned at its replacement insert), other writes
// that reference the user still go through. On PostgreSQL, inserting a row
// with a foreign key to users (here a group membership) takes FOR KEY SHARE
// on the user's row, which a plain FOR UPDATE blocks for the whole rotation
// transaction; the lock rotation and revocation take is FOR NO KEY UPDATE,
// which still excludes itself (so they stay serialized) but not FOR KEY SHARE.
//
// PostgreSQL only: MySQL has no exclusive row lock weaker than FOR UPDATE to
// ask for, so there is nothing to assert there (this test does not run
// against it, and does not claim MySQL behaves either way).
func TestFault_Concurrency_RotationLockAllowsForeignKeyInserts(t *testing.T) {
	var dbs []faulttest.TestDB
	for _, db := range faultDBs {
		if db.Kind == database.DriverPostgres {
			dbs = append(dbs, db)
		}
	}
	if len(dbs) == 0 {
		t.Skip("needs PostgreSQL (-auth.postgres-dsn)")
	}
	faulttest.ForEachDB(t, dbs, func(t *testing.T, kind database.Driver, dsn string) {
		release := make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		insert := faulttest.Sequence(faulttest.Block(release))
		faults := &faulttest.DBFaults{Statement: insert, Match: insertsRefreshToken}
		f := newRotationFixture(t, kind, dsn, faults)
		ctx := context.Background()
		faults.Disarm()
		group, err := auth.EnsureGroup(ctx, f.db.DB, "fk-insert-group")
		if err != nil {
			t.Fatal(err)
		}
		faults.Arm()

		rotated := make(chan error, 1)
		go func() {
			_, err := f.svc.RotateRefresh(ctx, f.refresh)
			rotated <- err
		}()
		wait(t, insert.Reached(1), "the rotation never reached its insert")

		// The rotation now holds the user's lock inside its open transaction.
		insertCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		err = f.db.WithContext(insertCtx).Exec(
			"INSERT INTO auth_user_groups (user_id, group_id) VALUES (?, ?)", f.user.ID, group.ID).Error
		if err != nil {
			t.Fatalf("a foreign-key insert for the rotating user = %v; the rotation's lock blocks writes that only reference the user", err)
		}
		close(release)
		if err := waitErr(t, rotated, "rotation"); err != nil {
			t.Fatalf("RotateRefresh = %v", err)
		}
	})
}

// nonSQLite is faultDBs without SQLite, whose single-connection pool cannot
// run two transactions at once.
func nonSQLite() []faulttest.TestDB {
	var dbs []faulttest.TestDB
	for _, db := range faultDBs {
		if db.Kind != database.DriverSQLite {
			dbs = append(dbs, db)
		}
	}
	return dbs
}

// locksUserRow matches the user-row lock rotation and revocation take
// first: a SELECT on users with FOR UPDATE (PostgreSQL: FOR NO KEY UPDATE).
func locksUserRow(query string) bool {
	return strings.Contains(query, "users") && forUpdate.MatchString(query)
}

var forUpdate = regexp.MustCompile(`(?i)\bFOR (NO KEY )?UPDATE\b`)

// readsActiveSessions matches the read of a user's active sessions: a list,
// or a revocation looking for the session it ends.
func readsActiveSessions(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "SELECT") && strings.Contains(query, "refresh_tokens") &&
		strings.Contains(query, "revoked_at IS NULL")
}

// readsActiveSessionsOrChainRow matches the read of a user's active sessions
// and each step of a walk along a session's chain.
func readsActiveSessionsOrChainRow(query string) bool {
	return readsActiveSessions(query) || readsChainRow(query)
}

// readsChainRow matches one step of a walk along a session's chain: a SELECT
// of one of the user's refresh_tokens rows by its id.
func readsChainRow(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "SELECT") && strings.Contains(query, "refresh_tokens") &&
		chainRowLookup.MatchString(query)
}

var chainRowLookup = regexp.MustCompile(`\bid = \S+ AND user_id = `)

// locksUserOrInsertsRefreshToken matches the user-row lock rotation and
// revocation take first, and the rotation's replacement-token insert.
func locksUserOrInsertsRefreshToken(query string) bool {
	return locksUserRow(query) || insertsRefreshToken(query)
}

// locksUserOrWritesRefreshTokens matches the user-row lock and every write to
// refresh_tokens: rotation's compare-and-swap UPDATE and replacement INSERT,
// and a revocation's UPDATE.
func locksUserOrWritesRefreshTokens(query string) bool {
	return locksUserRow(query) || updatesRefreshTokens(query) || insertsRefreshToken(query)
}

// updatesRefreshTokens matches an UPDATE of refresh_tokens: a rotation's
// compare-and-swap, a revocation, a logout.
func updatesRefreshTokens(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "UPDATE") && strings.Contains(query, "refresh_tokens")
}

func wait(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("the %s is wedged", what)
		return nil
	}
}
