package auth_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York must load on a machine without zone data

	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func TestSessionsServiceSQLite(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "sessions.db") + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	runSessionsService(t, db)
}

// sessionsEnv is a service over db plus helpers to drive its sessions.
type sessionsEnv struct {
	svc *auth.Service
	ctx context.Context
	tag string
}

func newSessionsEnv(t *testing.T, db *database.DB, tag string, access, refresh time.Duration) sessionsEnv {
	t.Helper()
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = access
	cfg.Auth.RefreshTokenTTL = refresh
	svc, err := auth.NewService(db.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sessionsEnv{svc: svc, ctx: context.Background(), tag: tag}
}

// user registers a user whose email is unique to this env, and logs it in n
// times.
func (e sessionsEnv) user(t *testing.T, name string, logins int) (auth.User, []auth.TokenPair) {
	t.Helper()
	user, err := e.svc.Register(e.ctx, fmt.Sprintf("%s-%s@example.com", name, e.tag), "correct-horse")
	if err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}
	pairs := make([]auth.TokenPair, logins)
	for i := range pairs {
		if pairs[i], err = e.svc.IssueTokens(e.ctx, user); err != nil {
			t.Fatalf("IssueTokens(%s): %v", name, err)
		}
	}
	return user, pairs
}

func (e sessionsEnv) authenticates(pair auth.TokenPair) bool {
	_, err := e.svc.ParseAccess(e.ctx, pair.AccessToken)
	return err == nil
}

func (e sessionsEnv) sessionID(t *testing.T, pair auth.TokenPair) string {
	t.Helper()
	id, err := auth.SessionIDOf(e.ctx, e.svc, pair.AccessToken)
	if err != nil {
		t.Fatalf("SessionIDOf: %v", err)
	}
	return id
}

func (e sessionsEnv) rotate(t *testing.T, pair auth.TokenPair) auth.TokenPair {
	t.Helper()
	next, err := e.svc.RotateRefresh(e.ctx, pair.RefreshToken)
	if err != nil {
		t.Fatalf("RotateRefresh: %v", err)
	}
	return next
}

// revokedAt returns when the refresh token row id was revoked, failing t
// unless it was.
func revokedAt(t *testing.T, db *database.DB, id uint) time.Time {
	t.Helper()
	var row auth.RefreshToken
	if err := db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	if row.RevokedAt == nil {
		t.Fatalf("refresh token row %d is not revoked", id)
	}
	return *row.RevokedAt
}

// runSessionsService runs the AUTH-5 service scenarios (listing, revocation,
// the reuse rule, session expiry) against db; SQLite, PostgreSQL and MySQL
// all run them.
func runSessionsService(t *testing.T, db *database.DB) {
	t.Helper()
	var n int
	newEnv := func(t *testing.T) sessionsEnv {
		t.Helper()
		n++
		return newSessionsEnv(t, db, fmt.Sprintf("%d-%d", time.Now().UnixNano(), n), time.Minute, time.Hour)
	}

	t.Run("list", func(t *testing.T) {
		e := newEnv(t)
		ada, a := e.user(t, "ada", 2)
		_, g := e.user(t, "grace", 1)
		sessions, err := e.svc.ListSessions(e.ctx, ada.ID)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		if len(sessions) != 2 {
			t.Fatalf("ListSessions(ada) = %d sessions, want 2 (grace's must not be listed)", len(sessions))
		}
		opaque := regexp.MustCompile(`^[0-9a-f]{32}$`)
		listed := map[string]bool{}
		for _, s := range sessions {
			if !opaque.MatchString(s.ID) {
				t.Errorf("session id = %q, want 32 lowercase hex characters", s.ID)
			}
			if _, err := strconv.Atoi(s.ID); err == nil {
				t.Errorf("session id = %q is a bare row id", s.ID)
			}
			if s.Current {
				t.Errorf("ListSessions set Current on %q without a request context", s.ID)
			}
			if !s.ExpiresAt.After(s.LastRefreshedAt) {
				t.Errorf("session %q expires_at %v not after last_refreshed_at %v", s.ID, s.ExpiresAt, s.LastRefreshedAt)
			}
			listed[s.ID] = true
		}
		for i, pair := range a {
			if id := e.sessionID(t, pair); !listed[id] {
				t.Errorf("session %d (id %q) missing from the list", i, id)
			}
		}
		if listed[e.sessionID(t, g[0])] {
			t.Error("another user's session is listed")
		}
		stale := e.sessionID(t, a[0])
		rotated := e.rotate(t, a[0])
		sessions, err = e.svc.ListSessions(e.ctx, ada.ID)
		if err != nil || len(sessions) != 2 {
			t.Fatalf("after a rotation ListSessions = %d sessions, %v; want 2", len(sessions), err)
		}
		if got := e.sessionID(t, rotated); got == stale || sessions[0].ID != got {
			t.Fatalf("after a rotation the session id is %q (was %q) and the first listed is %q; want a new id, listed first", got, stale, sessions[0].ID)
		}
	})

	// The id is the session's row id under an HMAC of the JWT secret, in a
	// domain of its own, so that no other MAC made with that secret (a CSRF
	// signature, say) can be mistaken for it. The domain is written out here,
	// not taken from the package, so changing it fails this test too.
	t.Run("session id derivation", func(t *testing.T) {
		e := newEnv(t)
		ada, _ := e.user(t, "ada", 1)
		var row auth.RefreshToken
		if err := db.Where("user_id = ?", ada.ID).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		sessions, err := e.svc.ListSessions(e.ctx, ada.ID)
		if err != nil || len(sessions) != 1 {
			t.Fatalf("ListSessions = %v, %v; want the one session", sessions, err)
		}
		mac := func(message string) string {
			h := hmac.New(sha256.New, []byte(testJWTSecret))
			h.Write([]byte(message))
			return hex.EncodeToString(h.Sum(nil)[:16])
		}
		rowID := strconv.FormatUint(uint64(row.ID), 10)
		want := mac("gombit/auth/session-id:" + rowID)
		if sessions[0].ID != want {
			t.Fatalf("session id = %q, want %q: the first 16 bytes, in hex, of HMAC-SHA256(secret, \"gombit/auth/session-id:\" + row id)", sessions[0].ID, want)
		}
		if undomained := mac(rowID); sessions[0].ID == undomained {
			t.Fatalf("session id %q is the HMAC of the bare row id: it has no domain of its own", sessions[0].ID)
		}
	})

	// Past the refresh TTL a session is not active: not listed.
	t.Run("list skips expired", func(t *testing.T) {
		e := newEnv(t)
		clock := &stepClock{now: time.Now()}
		auth.SetClock(e.svc, clock)
		ada, _ := e.user(t, "ada", 1)
		clock.now = clock.now.Add(time.Hour + time.Minute)
		if sessions, err := e.svc.ListSessions(e.ctx, ada.ID); err != nil || len(sessions) != 0 {
			t.Fatalf("ListSessions past the refresh TTL = %v, %v; want none", sessions, err)
		}
	})

	// last_refreshed_at and expires_at both come from the Service's Clock, not
	// one of them from GORM's.
	t.Run("list uses the service clock", func(t *testing.T) {
		e := newEnv(t)
		issuedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		auth.SetClock(e.svc, &stepClock{now: issuedAt})
		ada, _ := e.user(t, "ada", 1)
		sessions, err := e.svc.ListSessions(e.ctx, ada.ID)
		if err != nil || len(sessions) != 1 {
			t.Fatalf("ListSessions = %v, %v; want one session", sessions, err)
		}
		if got := sessions[0].LastRefreshedAt; !got.Equal(issuedAt) {
			t.Errorf("last_refreshed_at = %v, want the service clock's %v", got, issuedAt)
		}
		if got, want := sessions[0].ExpiresAt, issuedAt.Add(time.Hour); !got.Equal(want) {
			t.Errorf("expires_at = %v, want %v", got, want)
		}
	})

	t.Run("revoke one", func(t *testing.T) {
		tests := []struct {
			name string
			// id picks the id ada revokes; it may rotate a session first.
			id      func(t *testing.T, e sessionsEnv, a []auth.TokenPair, g auth.TokenPair) string
			wantErr error
			wantB   bool // whether ada's second session survives
		}{
			{"named session", func(t *testing.T, e sessionsEnv, a []auth.TokenPair, _ auth.TokenPair) string {
				return e.sessionID(t, a[1])
			}, nil, false},
			{"unknown id", func(*testing.T, sessionsEnv, []auth.TokenPair, auth.TokenPair) string {
				return "00000000000000000000000000000000"
			}, auth.ErrSessionNotFound, true},
			{"another user's session", func(t *testing.T, e sessionsEnv, _ []auth.TokenPair, g auth.TokenPair) string {
				return e.sessionID(t, g)
			}, auth.ErrSessionNotFound, true},
			{"id listed before the session refreshed", func(t *testing.T, e sessionsEnv, a []auth.TokenPair, _ auth.TokenPair) string {
				stale := e.sessionID(t, a[1])
				e.rotate(t, a[1])
				return stale
			}, auth.ErrSessionNotFound, true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				e := newEnv(t)
				ada, a := e.user(t, "ada", 2)
				_, g := e.user(t, "grace", 1)
				id := tt.id(t, e, a, g[0])
				if err := e.svc.RevokeSession(e.ctx, ada.ID, id); !errors.Is(err, tt.wantErr) {
					t.Fatalf("RevokeSession(%q) = %v, want %v", id, err, tt.wantErr)
				}
				if !e.authenticates(a[0]) || !e.authenticates(g[0]) {
					t.Fatal("a session the revocation did not name was revoked")
				}
				if tt.wantErr == nil && e.authenticates(a[1]) {
					t.Fatal("the named session still authenticates")
				}
			})
		}
		t.Run("a user that does not exist", func(t *testing.T) {
			e := newEnv(t)
			if err := e.svc.RevokeSession(e.ctx, 987654321, "00000000000000000000000000000000"); !errors.Is(err, auth.ErrSessionNotFound) {
				t.Fatalf("RevokeSession(missing user) = %v, want ErrSessionNotFound", err)
			}
		})
	})

	// The session that asks to end the others keeps its whole chain, however
	// many times it rotated since its access token was checked. A session
	// that had already ended keeps the instant it ended.
	t.Run("revoke others", func(t *testing.T) {
		for _, rotations := range []int{0, 1, 2, 3} {
			t.Run(fmt.Sprintf("%d rotations after authentication", rotations), func(t *testing.T) {
				e := newEnv(t)
				clock := &stepClock{now: time.Now()}
				auth.SetClock(e.svc, clock)
				ada, a := e.user(t, "ada", 3)
				_, g := e.user(t, "grace", 1)
				// The request authenticated as a[0] before the rotations.
				current, err := auth.AuthenticatedRow(e.ctx, e.svc, a[0].AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				// a[2] logged out before the request.
				loggedOutRow, err := auth.AuthenticatedRow(e.ctx, e.svc, a[2].AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				if err := e.svc.RevokeRefresh(e.ctx, a[2].RefreshToken); err != nil {
					t.Fatal(err)
				}
				loggedOut := revokedAt(t, db, loggedOutRow)
				head := a[0]
				for i := 0; i < rotations; i++ {
					head = e.rotate(t, head)
				}
				clock.now = clock.now.Add(30 * time.Second)
				if err := auth.RevokeOtherSessionsKeeping(e.ctx, e.svc, ada.ID, current); err != nil {
					t.Fatalf("revoke others: %v", err)
				}
				if !e.authenticates(head) {
					t.Fatal("the current session's head was revoked")
				}
				e.rotate(t, head) // and it can still refresh
				if e.authenticates(a[1]) {
					t.Fatal("the other session still authenticates")
				}
				if !e.authenticates(g[0]) {
					t.Fatal("another user's session was revoked")
				}
				if again := revokedAt(t, db, loggedOutRow); !again.Equal(loggedOut) {
					t.Fatalf("the session that had logged out at %v now ended at %v; revoking the others must leave it as it was", loggedOut, again)
				}
			})
		}
	})

	t.Run("revoke all", func(t *testing.T) {
		e := newEnv(t)
		ada, a := e.user(t, "ada", 2)
		_, g := e.user(t, "grace", 1)
		if err := e.svc.RevokeAllSessions(e.ctx, ada.ID); err != nil {
			t.Fatalf("RevokeAllSessions: %v", err)
		}
		if e.authenticates(a[0]) || e.authenticates(a[1]) || !e.authenticates(g[0]) {
			t.Fatalf("authenticates a0=%v a1=%v g=%v, want false false true",
				e.authenticates(a[0]), e.authenticates(a[1]), e.authenticates(g[0]))
		}
		if err := e.svc.RevokeAllSessions(e.ctx, 987654321); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("RevokeAllSessions(missing user) = %v, want ErrSessionNotFound", err)
		}
	})

	// A route that revokes one session reports whether it ended the
	// request's own: the session's current row, reached along its chain from
	// the row the request was authenticated as, as ListSessions marks it
	// current. A request authenticated as R, whose session has refreshed into
	// H since, ends its own session when it revokes H, and not when it
	// revokes another.
	t.Run("revoke one ends the requester's session along its chain", func(t *testing.T) {
		e := newEnv(t)
		ada, a := e.user(t, "ada", 2)
		requester, err := auth.AuthenticatedRow(e.ctx, e.svc, a[0].AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		head := e.rotate(t, a[0])
		headID, otherID := e.sessionID(t, head), e.sessionID(t, a[1])
		if ended, err := auth.RevokeSessionAs(e.ctx, e.svc, ada.ID, requester, otherID); err != nil || ended {
			t.Fatalf("revoking another session on behalf of R = %v, %v; want it ended, not the requester's own", ended, err)
		}
		if e.authenticates(a[1]) {
			t.Fatal("the other session still authenticates")
		}
		if ended, err := auth.RevokeSessionAs(e.ctx, e.svc, ada.ID, requester, headID); err != nil || !ended {
			t.Fatalf("revoking H on behalf of R = %v, %v; want the requester's own session ended", ended, err)
		}
		if e.authenticates(head) {
			t.Fatal("H still authenticates")
		}
	})

	// RevokeSession and RevokeAllSessions do not look at the caller's own
	// session. Application code that revokes a user's sessions, an admin
	// action say, runs with its own request's session in its context: one of
	// another user, maybe ended by now. They revoke all the same.
	t.Run("exported revocations ignore the caller's session", func(t *testing.T) {
		for _, callerEnded := range []bool{false, true} {
			t.Run(fmt.Sprintf("caller's session ended %v", callerEnded), func(t *testing.T) {
				e := newEnv(t)
				admin, adminPairs := e.user(t, "admin", 1)
				adminRow, err := auth.AuthenticatedRow(e.ctx, e.svc, adminPairs[0].AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				if callerEnded {
					if err := e.svc.RevokeAllSessions(e.ctx, admin.ID); err != nil {
						t.Fatal(err)
					}
				}
				ada, a := e.user(t, "ada", 2)
				ctx := auth.ContextWithSession(e.ctx, admin, adminRow)
				if err := e.svc.RevokeSession(ctx, ada.ID, e.sessionID(t, a[0])); err != nil {
					t.Fatalf("RevokeSession with the admin's session in ctx = %v", err)
				}
				if e.authenticates(a[0]) {
					t.Fatal("the session RevokeSession named still authenticates")
				}
				if err := e.svc.RevokeAllSessions(ctx, ada.ID); err != nil {
					t.Fatalf("RevokeAllSessions with the admin's session in ctx = %v", err)
				}
				if e.authenticates(a[1]) {
					t.Fatal("a session still authenticates after RevokeAllSessions")
				}
			})
		}
	})

	// A revocation on behalf of a session that has ended refuses before it
	// looks the target up: ErrSessionEnded (a 401 over the routes), whatever
	// the id names, an unknown id and the stale id of a session that has
	// refreshed included, and nothing is revoked. Here the session ended
	// before the call, revoked or expired; the fault tests end it while the
	// call waits for the user's lock.
	t.Run("revocations on behalf of an ended session", func(t *testing.T) {
		for _, ending := range []string{"revoked", "expired"} {
			t.Run(ending, func(t *testing.T) {
				e := newEnv(t)
				start := time.Now().Truncate(time.Second)
				clock := &stepClock{now: start}
				auth.SetClock(e.svc, clock)
				// R asks. O and S sign in half an hour later, so they outlive R,
				// and S refreshes, which leaves its first id stale.
				ada, r := e.user(t, "ada", 1)
				requester, err := auth.AuthenticatedRow(e.ctx, e.svc, r[0].AccessToken)
				if err != nil {
					t.Fatal(err)
				}
				rID := e.sessionID(t, r[0])
				clock.now = start.Add(30 * time.Minute)
				o, err := e.svc.IssueTokens(e.ctx, ada)
				if err != nil {
					t.Fatal(err)
				}
				s, err := e.svc.IssueTokens(e.ctx, ada)
				if err != nil {
					t.Fatal(err)
				}
				oID, staleID := e.sessionID(t, o), e.sessionID(t, s)
				headID := e.sessionID(t, e.rotate(t, s))
				if ending == "revoked" {
					if err := e.svc.RevokeSession(e.ctx, ada.ID, rID); err != nil {
						t.Fatal(err)
					}
				} else {
					clock.now = start.Add(time.Hour + time.Second) // R's refresh TTL is an hour
				}
				calls := map[string]func() error{
					"revoke an unknown id": func() error {
						_, err := auth.RevokeSessionAs(e.ctx, e.svc, ada.ID, requester, "00000000000000000000000000000000")
						return err
					},
					"revoke a stale id": func() error {
						_, err := auth.RevokeSessionAs(e.ctx, e.svc, ada.ID, requester, staleID)
						return err
					},
					"revoke an active session": func() error {
						_, err := auth.RevokeSessionAs(e.ctx, e.svc, ada.ID, requester, oID)
						return err
					},
					"revoke the others": func() error {
						return auth.RevokeOtherSessionsKeeping(e.ctx, e.svc, ada.ID, requester)
					},
					"revoke all": func() error {
						return auth.RevokeAllSessionsAs(e.ctx, e.svc, ada.ID, requester)
					},
				}
				for name, call := range calls {
					if err := call(); !errors.Is(err, auth.ErrSessionEnded) {
						t.Errorf("%s on behalf of a %s session = %v, want ErrSessionEnded", name, ending, err)
					}
				}
				sessions, err := e.svc.ListSessions(e.ctx, ada.ID)
				if err != nil {
					t.Fatal(err)
				}
				listed := map[string]bool{}
				for _, session := range sessions {
					listed[session.ID] = true
				}
				if len(sessions) != 2 || !listed[oID] || !listed[headID] {
					t.Fatalf("after the refused revocations the user has %d active sessions %v; want only O and S's head", len(sessions), listed)
				}
			})
		}
	})

	// Only a rotated refresh token is evidence of theft. One revoked by
	// logout or session revocation is simply invalid.
	t.Run("reuse rule", func(t *testing.T) {
		tests := []struct {
			name      string
			present   func(t *testing.T, e sessionsEnv, ada auth.User, b auth.TokenPair)
			wantErr   error
			wantAlive bool // whether ada's other session survives
		}{
			{"logged-out token", func(t *testing.T, e sessionsEnv, _ auth.User, b auth.TokenPair) {
				if err := e.svc.RevokeRefresh(e.ctx, b.RefreshToken); err != nil {
					t.Fatal(err)
				}
			}, auth.ErrInvalidRefreshToken, true},
			{"token of a revoked session", func(t *testing.T, e sessionsEnv, ada auth.User, b auth.TokenPair) {
				if err := e.svc.RevokeSession(e.ctx, ada.ID, e.sessionID(t, b)); err != nil {
					t.Fatal(err)
				}
			}, auth.ErrInvalidRefreshToken, true},
			{"already-rotated token", func(t *testing.T, e sessionsEnv, _ auth.User, b auth.TokenPair) {
				e.rotate(t, b)
			}, auth.ErrRefreshReuse, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				e := newEnv(t)
				ada, a := e.user(t, "ada", 2)
				_, g := e.user(t, "grace", 1)
				tt.present(t, e, ada, a[1])
				if _, err := e.svc.RotateRefresh(e.ctx, a[1].RefreshToken); !errors.Is(err, tt.wantErr) {
					t.Fatalf("RotateRefresh(presented again) = %v, want %v", err, tt.wantErr)
				}
				if got := e.authenticates(a[0]); got != tt.wantAlive {
					t.Fatalf("ada's other session authenticates = %v, want %v", got, tt.wantAlive)
				}
				if !e.authenticates(g[0]) {
					t.Fatal("another user's session was revoked")
				}
			})
		}
	})

	// A session's times are compared as instants, whatever offset the clock's
	// location gave them. In America/New_York the clocks went back on
	// 2026-11-01 (02:00 EDT, -04:00, became 01:00 EST, -05:00), so 01:20-05:00
	// is later than 01:50-04:00 though the text sorts the other way. SQLite
	// stores a time as text with the offset it was written with; a comparison
	// or an ORDER BY done there gets these cases wrong.
	t.Run("times across a daylight-saving change", func(t *testing.T) {
		ny, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatalf("LoadLocation(America/New_York): %v", err)
		}
		// at is 2026-11-01 hh:mm UTC, shown in New York (EDT until 06:00 UTC).
		at := func(hour, minute int) time.Time {
			return time.Date(2026, time.November, 1, hour, minute, 0, 0, time.UTC).In(ny)
		}
		if _, offset := at(5, 50).Zone(); offset != -4*3600 {
			t.Fatalf("05:50 UTC is at offset %d in New York, want EDT: the test's premise is wrong", offset)
		}
		if _, offset := at(6, 20).Zone(); offset != -5*3600 {
			t.Fatalf("06:20 UTC is at offset %d in New York, want EST: the test's premise is wrong", offset)
		}

		t.Run("an expired session is not listed or revocable", func(t *testing.T) {
			e := newEnv(t)
			clock := &stepClock{now: at(4, 50)} // expires 05:50 UTC = 01:50-04:00
			auth.SetClock(e.svc, clock)
			ada, a := e.user(t, "ada", 1)
			id := e.sessionID(t, a[0])
			clock.now = at(6, 20) // 30 minutes later: 01:20-05:00
			if sessions, err := e.svc.ListSessions(e.ctx, ada.ID); err != nil || len(sessions) != 0 {
				t.Fatalf("ListSessions after the session expired = %v, %v; want none", sessions, err)
			}
			if err := e.svc.RevokeSession(e.ctx, ada.ID, id); !errors.Is(err, auth.ErrSessionNotFound) {
				t.Fatalf("RevokeSession of an expired session = %v, want ErrSessionNotFound", err)
			}
		})

		t.Run("an active session is listed and revocable", func(t *testing.T) {
			e := newEnv(t)
			clock := &stepClock{now: at(5, 20)} // expires 06:20 UTC = 01:20-05:00
			auth.SetClock(e.svc, clock)
			ada, a := e.user(t, "ada", 1)
			id := e.sessionID(t, a[0])
			clock.now = at(5, 50) // still 30 minutes to go: 01:50-04:00
			sessions, err := e.svc.ListSessions(e.ctx, ada.ID)
			if err != nil || len(sessions) != 1 || sessions[0].ID != id {
				t.Fatalf("ListSessions before the session expires = %v, %v; want it listed as %q", sessions, err, id)
			}
			if err := e.svc.RevokeSession(e.ctx, ada.ID, id); err != nil {
				t.Fatalf("RevokeSession of an active session = %v", err)
			}
		})

		t.Run("the later session is listed first", func(t *testing.T) {
			e := newEnv(t)
			clock := &stepClock{now: at(5, 40)} // 01:40-04:00
			auth.SetClock(e.svc, clock)
			ada, first := e.user(t, "ada", 1)
			firstID := e.sessionID(t, first[0])
			clock.now = at(6, 10) // 30 minutes later: 01:10-05:00
			second, err := e.svc.IssueTokens(e.ctx, ada)
			if err != nil {
				t.Fatal(err)
			}
			secondID := e.sessionID(t, second)
			sessions, err := e.svc.ListSessions(e.ctx, ada.ID)
			if err != nil || len(sessions) != 2 {
				t.Fatalf("ListSessions = %v, %v; want both sessions", sessions, err)
			}
			if sessions[0].ID != secondID || sessions[1].ID != firstID {
				t.Fatalf("listed %q then %q; want the later session %q first, then %q", sessions[0].ID, sessions[1].ID, secondID, firstID)
			}
		})
	})

	// Config.Validate accepts an access TTL above the refresh TTL; the access
	// token must not outlive the session it belongs to.
	t.Run("access token ends with its session", func(t *testing.T) {
		e := newSessionsEnv(t, db, fmt.Sprintf("a2-%d", time.Now().UnixNano()), time.Hour, time.Minute)
		clock := &stepClock{now: time.Now()}
		auth.SetClock(e.svc, clock)
		ada, a := e.user(t, "ada", 1)
		if !e.authenticates(a[0]) {
			t.Fatal("the access token is rejected while its session is active")
		}
		clock.now = clock.now.Add(2 * time.Minute)
		if _, err := e.svc.ParseAccess(e.ctx, a[0].AccessToken); !errors.Is(err, auth.ErrInvalidAccessToken) {
			t.Fatalf("ParseAccess after the session expired = %v, want ErrInvalidAccessToken", err)
		}
		if sessions, err := e.svc.ListSessions(e.ctx, ada.ID); err != nil || len(sessions) != 0 {
			t.Fatalf("ListSessions after expiry = %v, %v; want none", sessions, err)
		}
	})
}
