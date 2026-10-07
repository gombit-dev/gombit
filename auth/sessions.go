package auth

import (
	"context"
	"crypto/hmac"
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
// the table, so both expose the same sessions (AUTH-5, #335).

var errSessionNotFound = errors.New("auth: session not found")

// errSessionEnded is returned by the revocations the sessions routes run on
// behalf of a request when the request's own session has ended by the time
// the revocation holds the user's lock: another request revoked it, or it
// expired, while this one waited. The gate authenticated the session before
// that wait, so the request must not act on it; the routes answer 401.
var errSessionEnded = errors.New("auth: the request's session has ended")

// ErrSessionNotFound is returned by RevokeSession when the id names no
// active session of the user, including a session that has refreshed since
// its id was listed or that a logout ended while the revocation ran, and by
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
	ID              string    `json:"id" example:"3f6c2a9e0b7d41c58e2f6a1d9c4b7e20" doc:"Opaque session identifier. It changes every time the session refreshes, so list again before revoking."`
	LastRefreshedAt time.Time `json:"last_refreshed_at" doc:"When the session last signed in or refreshed its tokens. Precision is roughly the access-token lifetime, not the last request."`
	ExpiresAt       time.Time `json:"expires_at" doc:"When the session ends unless it refreshes first"`
	Current         bool      `json:"current" example:"true" doc:"True for the session that made this request"`
}

// sessionID derives the opaque id the API shows for the refresh token row
// refreshID. Row ids are sequential, so the sessions API does not expose
// them (that would reveal how many logins the whole application has seen).
// The access JWT's rid claim is still the row id, readable by whoever holds
// the token: the opaque id keeps row ids out of the API's responses and
// paths, it does not hide them from a client that decodes its own token.
func (s *Service) sessionID(refreshID uint) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(sessionIDDomain + strconv.FormatUint(uint64(refreshID), 10)))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// activeSessionRows returns the user's active sessions, most recently
// refreshed first. Expiry and order are decided here, on instants, not in SQL:
// SQLite stores a time as text carrying the offset it was written with, so
// comparing or sorting those strings goes wrong when the clock's offset
// changed between two writes (a daylight-saving change).
func (s *Service) activeSessionRows(tx *gorm.DB, userID uint) ([]RefreshToken, error) {
	var unrevoked []RefreshToken
	if err := tx.Where("user_id = ? AND revoked_at IS NULL", userID).Find(&unrevoked).Error; err != nil {
		return nil, err
	}
	now := s.now()
	rows := unrevoked[:0]
	for _, row := range unrevoked {
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
			ID:              s.sessionID(row.ID),
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
// further along its chain is the session's. rows are read before the chain:
// a rotation after that only extends the chain past the listed row, which
// stays on it.
func sessionRow(tx *gorm.DB, userID, rid uint, rows []RefreshToken) (uint, error) {
	listed := make(map[uint]bool, len(rows))
	for _, row := range rows {
		if row.ID == rid {
			return rid, nil
		}
		listed[row.ID] = true
	}
	chain, _, err := sessionChain(tx, userID, rid)
	if err != nil {
		return 0, err
	}
	for _, id := range chain {
		if listed[id] {
			return id, nil
		}
	}
	return 0, nil
}

// RevokeSession ends one of the user's active sessions. It returns
// ErrSessionNotFound when id names no active session of that user; a
// session that refreshed after id was listed has a new id, so the caller
// lists again rather than having the revoke silently miss. It also returns
// ErrSessionNotFound when the session's row turns out revoked by the time it
// is updated (a logout, which does not take the user's lock, ended it
// first), so a success always means this call ended the session. A user that
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
			if hmac.Equal([]byte(s.sessionID(row.ID)), []byte(id)) {
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
// user's lock (requesterChain).
func (s *Service) revokeAllSessionsAs(ctx context.Context, userID, requester uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		now := s.now()
		if requester != 0 {
			if _, err := requesterChain(tx, userID, requester, now); err != nil {
				return err
			}
		}
		return revokeAllTx(tx, userID, now)
	})
}

// lockSessionsError maps a failed lockUserTx in a revocation: a user that
// does not exist has no sessions to revoke.
func lockSessionsError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errSessionNotFound
	}
	return err
}

// maxChainHops bounds how far sessionChain follows replaced_by. The chain
// past a request's row only grows by the rotations that happened since the
// request was authenticated, so the bound is a backstop, not a limit anything
// reaches.
const maxChainHops = 1000

// sessionChain follows the chain of the user's refresh token rows from
// fromID: fromID, the row that replaced it, the row that replaced that one,
// and so on to the row nothing has replaced yet. It returns the ids it
// followed and the last row it read, which is that row, or the last one it
// could reach; last is nil when fromID names no row of the user. A cycle
// (which rotation never writes) or an over-long chain stops the walk rather
// than looping.
func sessionChain(tx *gorm.DB, userID, fromID uint) (ids []uint, last *RefreshToken, err error) {
	seen := map[uint]bool{fromID: true}
	ids = []uint{fromID}
	next := fromID
	for hop := 0; hop < maxChainHops; hop++ {
		var row RefreshToken
		err := tx.Where("id = ? AND user_id = ?", next, userID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		last = &row
		if row.ReplacedBy == nil || seen[*row.ReplacedBy] {
			break
		}
		next = *row.ReplacedBy
		seen[next] = true
		ids = append(ids, next)
	}
	return ids, last, nil
}

// requesterChain is sessionChain for the request's own session, from the row
// rid the gate authenticated, and fails with errSessionEnded unless the last
// row it reads, the session's head, is active at now (unrevoked and
// unexpired). The gate checked the session before the revocation waited for
// the user's lock, and another revocation, or the clock, may have ended it
// during that wait: tx must hold lockUserTx, and now be read after it.
func requesterChain(tx *gorm.DB, userID, rid uint, now time.Time) ([]uint, error) {
	ids, head, err := sessionChain(tx, userID, rid)
	if err != nil {
		return nil, err
	}
	if head == nil || head.RevokedAt != nil || !head.ExpiresAt.After(now) {
		return nil, errSessionEnded
	}
	return ids, nil
}

// revokeOtherSessions ends every active session of the user except the one
// whose chain includes the refresh token row keepID: the row the request's
// access token named, which may have rotated any number of times since the
// middleware checked it. With the user lock held no further rotation can
// start, so it keeps every row of the chain from keepID on, the session's
// current head included. It returns errSessionEnded, and ends nothing, when
// that session has itself ended by then (requesterChain).
func (s *Service) revokeOtherSessions(ctx context.Context, userID, keepID uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		now := s.now()
		keepIDs, err := requesterChain(tx, userID, keepID, now)
		if err != nil {
			return err
		}
		return tx.Model(&RefreshToken{}).
			Where("user_id = ? AND revoked_at IS NULL AND id NOT IN ?", userID, keepIDs).
			Update("revoked_at", now).Error
	})
}
