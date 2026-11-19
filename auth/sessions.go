package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// A session is one sign-in: the chain of refresh tokens a login starts and
// each rotation extends. Only the head of a chain is unrevoked, so the
// active sessions are exactly the unrevoked, unexpired refresh_tokens rows,
// and revoking a session is revoking its head. Bearer and cookie mode share
// the table, so both expose the same sessions (AUTH-5, #335). Every row of a
// session carries its SessionID, which the login draws and rotation copies:
// the session's id in the API, unchanged by refreshes, and how any row of
// the session finds its head in one read (sessionHead; AUTH-11, #661).

var errSessionNotFound = errors.New("auth: session not found")

// errSessionEnded is returned by the revocations the sessions routes run on
// behalf of a request when the request's own session has ended by the time
// the revocation holds the user's lock: another request revoked it, or it
// expired, while this one waited. The gate authenticated the session before
// that wait, so the request must not act on it; the routes answer 401.
var errSessionEnded = errors.New("auth: the request's session has ended")

// errChainUnresolved is a search for a session's head (sessionHead) that did
// not find it: a session whose SessionID no unrotated row has, or, along a
// chain of rows from before SessionID existed, maxChainHops rows read
// without reaching it, a cycle, which rotation never writes, or a successor
// that no longer exists (replaced_by has no foreign key: a manual delete or
// a partial restore). The row it stopped at is no head, so a caller must
// fail rather than take it for one (a logout would end nothing and answer
// success, #466).
var errChainUnresolved = errors.New("auth: the refresh token chain does not reach its session's head")

// ErrSessionNotFound is returned by RevokeSession when the id names no
// active session of the user, including a session that a logout ended while
// the revocation ran, and by
// RevokeSession and RevokeAllSessions when the user does not exist (a user
// that is not there has no sessions).
var ErrSessionNotFound = errSessionNotFound

// sessionIDDomain separates session-id MACs from every other use of the JWT
// secret (access JWTs, CSRF token signatures).
const sessionIDDomain = "gombit/auth/session-id:"

// AuthSession is one active sign-in as the sessions API lists it. Huma names
// a schema after its Go type, in the one namespace every operation of the app
// shares, and two types with the same name panic at boot: the Auth prefix
// keeps it clear of an application's own Session type.
type AuthSession struct {
	ID              string    `json:"id" example:"3f6c2a9e0b7d41c58e2f6a1d9c4b7e20" doc:"Opaque session identifier, the same for the session's whole life: refreshing does not change it"`
	LastRefreshedAt time.Time `json:"last_refreshed_at" doc:"When the session last signed in or refreshed its tokens. Precision is roughly the access-token lifetime, not the last request."`
	ExpiresAt       time.Time `json:"expires_at" doc:"When the session ends unless it refreshes first"`
	Current         bool      `json:"current" example:"true" doc:"True for the session that made this request"`
}

// sessionIDBytes is the size of a session's random SessionID, hex-encoded
// to 32 characters (RefreshToken.SessionID's column size).
const sessionIDBytes = 16

// newSessionID draws a session's SessionID.
func newSessionID() (string, error) {
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// sessionIDOf is the id the API shows for the session row belongs to: its
// SessionID, the same for every row of the session. A row from before
// SessionID existed has none, and gets the id the sessions API showed then,
// derived from its row id (legacySessionID), until its next rotation gives
// its session a SessionID: the id of such a session changes that once.
func (s *Service) sessionIDOf(row RefreshToken) string {
	if row.SessionID != "" {
		return row.SessionID
	}
	return s.legacySessionID(row.ID)
}

// legacySessionID derives the opaque id the API shows for refresh token row
// refreshID when the row has no SessionID. Row ids are sequential, so the
// sessions API does not expose them (that would reveal how many logins the
// whole application has seen). The access JWT's rid claim is still the row
// id, readable by whoever holds the token: the opaque id keeps row ids out
// of the API's responses and paths, it does not hide them from a client that
// decodes its own token.
func (s *Service) legacySessionID(refreshID uint) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(sessionIDDomain + strconv.FormatUint(uint64(refreshID), 10)))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// expiryPrefilterSlack sets how far before now the SQL in activeSessionRows
// cuts off expires_at; its loop makes the exact cut. It is more than any two
// UTC offsets differ (26h, UTC-12 to UTC+14), the most SQLite's comparison of
// times as text can be off by, so the cut never leaves out an active session.
// On SQLite the cut itself moves by that error: it can leave out a row that
// expired a day ago, or read one that expired three days ago.
const expiryPrefilterSlack = 48 * time.Hour

// activeSessionRows returns the user's active sessions, most recently
// refreshed first. The SQL leaves out the rows that expired well before now
// (expiryPrefilterSlack), on the index on expires_at, since nothing prunes
// expired rows yet; the exact expiry and the order are decided here, on
// instants. SQLite stores a time as text carrying the offset it was written
// with and compares or sorts those strings, which is off by the difference
// between the two offsets, in either direction, when the clock's offset
// changed between two writes (a daylight-saving change): a bare
// expires_at > now could leave out a session that is still active.
func (s *Service) activeSessionRows(tx *gorm.DB, userID uint) ([]RefreshToken, error) {
	now := s.now()
	var candidates []RefreshToken
	if err := tx.Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", userID, now.Add(-expiryPrefilterSlack)).
		Find(&candidates).Error; err != nil {
		return nil, err
	}
	rows := candidates[:0]
	for _, row := range candidates {
		if row.ExpiresAt.After(now) {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	return rows, nil
}

// ListSessions returns the user's active sessions, most recently refreshed
// first. When ctx comes from a request the auth middlewares authenticated,
// the request's own session has Current set, also when it refreshed while the
// request ran; otherwise none does.
func (s *Service) ListSessions(ctx context.Context, userID uint) ([]AuthSession, error) {
	db := s.db.WithContext(ctx)
	rows, err := s.activeSessionRows(db, userID)
	if err != nil {
		return nil, err
	}
	current, err := currentSessionRow(ctx, db, userID, rows)
	if err != nil {
		return nil, err
	}
	sessions := make([]AuthSession, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, AuthSession{
			ID:              s.sessionIDOf(row),
			LastRefreshedAt: row.CreatedAt,
			ExpiresAt:       row.ExpiresAt,
			Current:         row.ID == current,
		})
	}
	return sessions, nil
}

// currentSessionRow returns the id of the row among rows that is the
// request's session, or 0 when ctx carries no session or rows do not include
// it (sessionRow, from the row the middleware recorded).
func currentSessionRow(ctx context.Context, tx *gorm.DB, userID uint, rows []RefreshToken) (uint, error) {
	rid, ok := sessionFromContext(ctx)
	if !ok {
		return 0, nil
	}
	return sessionRow(tx, userID, rid, rows)
}

// sessionRow returns the id of the row among rows that is the session of the
// refresh token row rid, or 0 when rows do not include it. rid is the row a
// request's access token names; the session may have refreshed since (the
// request raced its own refresh), so when rid is not listed, the listed row
// of the same session is: the one with rid's SessionID, or, from a row that
// predates SessionID, one further along its chain. rows may be read before a
// rotation the walk then sees, so the listed row is matched by session, not
// by being the head now.
func sessionRow(tx *gorm.DB, userID, rid uint, rows []RefreshToken) (uint, error) {
	listed := make(map[uint]bool, len(rows))
	bySession := make(map[string]uint, len(rows))
	for _, row := range rows {
		if row.ID == rid {
			return rid, nil
		}
		listed[row.ID] = true
		if row.SessionID != "" {
			bySession[row.SessionID] = row.ID
		}
	}
	var found uint
	_, err := walkSession(tx, userID, rid, func(row RefreshToken) bool {
		if listed[row.ID] {
			found = row.ID
			return true
		}
		if row.SessionID != "" {
			found = bySession[row.SessionID]
			return true // the rest of the session has this SessionID too
		}
		return false
	})
	if err != nil {
		return 0, err
	}
	return found, nil
}

// RevokeSession ends one of the user's active sessions. It returns
// ErrSessionNotFound when id names no active session of that user; a
// session's id does not change when it refreshes, so an id listed before a
// refresh still ends it. It also returns
// ErrSessionNotFound when the session's row turns out revoked by the time it
// is updated, so a success always means this call ended the session (every
// revocation, logout included, takes the user's lock, so one that ended the
// session first already left it out of the active sessions). A user that
// does not exist also yields ErrSessionNotFound. It does not look at the
// caller's own session, which application code (an admin action revoking
// another user's session, say) does not have; the route checks the
// request's (revokeSessionAs).
func (s *Service) RevokeSession(ctx context.Context, userID uint, id string) error {
	_, err := s.revokeSessionAs(ctx, userID, 0, id)
	return err
}

// revokeSessionAs is RevokeSession on behalf of the request's session,
// authenticated as the refresh token row requester, or of no session when
// requester is 0. Under the user's lock it first finds the requester's
// session among the active ones, as ListSessions marks it current
// (sessionRow), and returns errSessionEnded, ending nothing, when it is not
// there: the session ended while the request waited for the lock. That
// comes before id is looked up, so a request from an ended session gets a
// 401, not a 404. endedCurrent reports whether the session it ended is the
// requester's own.
func (s *Service) revokeSessionAs(ctx context.Context, userID, requester uint, id string) (endedCurrent bool, err error) {
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		rows, err := s.activeSessionRows(tx, userID)
		if err != nil {
			return err
		}
		var own uint
		if requester != 0 {
			if own, err = sessionRow(tx, userID, requester, rows); err != nil {
				return err
			}
			if own == 0 {
				return errSessionEnded
			}
		}
		for _, row := range rows {
			if hmac.Equal([]byte(s.sessionIDOf(row)), []byte(id)) {
				result := tx.Model(&RefreshToken{}).
					Where("id = ? AND revoked_at IS NULL", row.ID).
					Update("revoked_at", s.now())
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected == 0 {
					return errSessionNotFound
				}
				endedCurrent = row.ID == own
				return nil
			}
		}
		return errSessionNotFound
	})
	if err != nil {
		return false, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditSessionRevoked, TargetUserID: uintPtr(userID), Details: details("scope", "one")})
	return endedCurrent, nil
}

// RevokeAllSessions ends every active session of the user. It returns
// ErrSessionNotFound when the user does not exist. Like RevokeSession, it
// does not look at the caller's own session; the route checks the request's
// (revokeAllSessionsAs).
func (s *Service) RevokeAllSessions(ctx context.Context, userID uint) error {
	return s.revokeAllSessionsAs(ctx, userID, 0)
}

// revokeAllSessionsAs is RevokeAllSessions on behalf of the request's
// session, authenticated as the refresh token row requester, or of no
// session when requester is 0. With a requester it returns errSessionEnded,
// and ends nothing, when that session has ended by the time it holds the
// user's lock (requesterSession).
func (s *Service) revokeAllSessionsAs(ctx context.Context, userID, requester uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		now := s.now()
		if requester != 0 {
			if _, err := requesterSession(tx, userID, requester, now); err != nil {
				return err
			}
		}
		return revokeAllTx(tx, userID, now)
	})
	if err == nil {
		s.Audit(ctx, AuditEvent{Action: AuditSessionRevoked, TargetUserID: uintPtr(userID), Details: details("scope", "all")})
	}
	return err
}

// lockSessionsError maps a failed lockUserTx in a revocation: a user that
// does not exist has no sessions to revoke.
func lockSessionsError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errSessionNotFound
	}
	return err
}

// maxChainHops bounds how many rows sessionHead reads, one SELECT each, along
// a chain of rows from before SessionID existed (a row with a SessionID finds
// its head in one read). It is a cost limit, not a guard against bad data
// (the cycle check already ends every walk): a logout walks from any
// unexpired token of the session, which can be RefreshTokenTTL / (rotation
// interval) rows behind its head (672 rows with the default TTLs and a
// refresh every 15 minutes). Such chains end with their tokens' expiry: every
// rotation since the upgrade has written a SessionID. A token maxChainHops or
// more rotations behind its head fails with errChainUnresolved (a 500) and
// ends nothing; such a session is ended by revoking it (DELETE
// /auth/sessions/{id}, or ?scope=all). A variable so tests can lower it.
var maxChainHops = 1000

// sessionHead returns the head of the session refresh token row fromID
// belongs to: the row nothing has replaced, which is the session's only row
// that can be unrevoked; it may itself be revoked or expired, a session that
// has ended. It returns nil when fromID names no row of the user. A row with a
// SessionID finds the head in one read: the user's unreplaced row with that
// SessionID. A row from before SessionID existed is followed along
// replaced_by to the head, or to the first row with a SessionID (its
// session's rotations since the upgrade). A session whose head is not found,
// a missing successor or a cycle (neither of which rotation writes), or
// maxChainHops rows read without reaching the head stop with
// errChainUnresolved rather than looping or returning a row that is not the
// head. tx must hold lockUserTx, or a rotation may move the head as it reads.
func sessionHead(tx *gorm.DB, userID, fromID uint) (*RefreshToken, error) {
	return walkSession(tx, userID, fromID, nil)
}

// walkSession is sessionHead, stopping early at the first row stop accepts
// (nil accepts none) and returning that row: stop sees each row read, from
// fromID's on, before the walk moves past it.
func walkSession(tx *gorm.DB, userID, fromID uint, stop func(RefreshToken) bool) (*RefreshToken, error) {
	seen := map[uint]bool{fromID: true}
	next := fromID
	for hop := 0; hop < maxChainHops; hop++ {
		var row RefreshToken
		err := tx.Where("id = ? AND user_id = ?", next, userID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if hop > 0 {
				return nil, errChainUnresolved // the previous row names a successor that is gone
			}
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if stop != nil && stop(row) {
			return &row, nil
		}
		if row.ReplacedBy == nil {
			return &row, nil
		}
		if row.SessionID != "" {
			return familyHead(tx, userID, row.SessionID)
		}
		if seen[*row.ReplacedBy] {
			return nil, errChainUnresolved
		}
		next = *row.ReplacedBy
		seen[next] = true
	}
	return nil, errChainUnresolved
}

// familyHead returns the head of the user's session sessionID: its row that
// nothing has replaced. Rotation sets the replaced row's replaced_by in the
// transaction that writes its successor, so the session has exactly one; the
// newest is taken should a restore have left more.
func familyHead(tx *gorm.DB, userID uint, sessionID string) (*RefreshToken, error) {
	var head RefreshToken
	err := tx.Where("user_id = ? AND session_id = ? AND replaced_by IS NULL", userID, sessionID).
		Order("id DESC").Limit(1).Find(&head).Error
	if err != nil {
		return nil, err
	}
	if head.ID == 0 {
		return nil, errChainUnresolved
	}
	return &head, nil
}

// requesterSession finds the request's own session, from the row rid the
// gate authenticated, and returns the rows to keep when ending the user's
// other sessions: its head, the session's one unrevoked row. It fails with
// errSessionEnded unless that head is active at now (unrevoked and
// unexpired). The gate checked the session before the revocation waited for
// the user's lock, and another revocation, or the clock, may have ended it
// during that wait: tx must hold lockUserTx, and now be read after it.
func requesterSession(tx *gorm.DB, userID, rid uint, now time.Time) ([]uint, error) {
	head, err := sessionHead(tx, userID, rid)
	if err != nil {
		return nil, err
	}
	if head == nil || head.RevokedAt != nil || !head.ExpiresAt.After(now) {
		return nil, errSessionEnded
	}
	return []uint{head.ID}, nil
}

// revokeOtherSessionsTx ends, in tx, every active session of the user but
// the one the refresh token row keepID belongs to (requesterSession): tx must
// hold lockUserTx, and now be read after it. It returns errSessionEnded, and
// ends nothing, when that session has itself ended.
func revokeOtherSessionsTx(tx *gorm.DB, userID, keepID uint, now time.Time) error {
	keep, err := requesterSession(tx, userID, keepID, now)
	if err != nil {
		return err
	}
	return revokeSessionsButTx(tx, userID, keep, now)
}

// revokeSessionsButTx ends every active refresh token row of the user but
// the rows keep (a session's head, from requesterSession), and voids the
// external sign-in codes not yet exchanged (each another session about to
// start).
func revokeSessionsButTx(tx *gorm.DB, userID uint, keep []uint, now time.Time) error {
	err := tx.Model(&RefreshToken{}).
		Where("user_id = ? AND revoked_at IS NULL AND id NOT IN ?", userID, keep).
		Update("revoked_at", now).Error
	if err != nil {
		return err
	}
	return voidTokensTx(tx, userID, 0, now, PurposeExternalLogin)
}

// revokeOtherSessions ends every active session of the user except the one
// the refresh token row keepID belongs to: the row the request's access
// token named, which may have rotated any number of times since the
// middleware checked it. With the user lock held no further rotation can
// start, so it keeps that session's current head. It returns
// errSessionEnded, and ends nothing, when that session has itself ended by
// then (requesterSession).
func (s *Service) revokeOtherSessions(ctx context.Context, userID, keepID uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		return revokeOtherSessionsTx(tx, userID, keepID, s.now())
	})
	if err == nil {
		s.Audit(ctx, AuditEvent{Action: AuditSessionRevoked, TargetUserID: uintPtr(userID), Details: details("scope", "others")})
	}
	return err
}
