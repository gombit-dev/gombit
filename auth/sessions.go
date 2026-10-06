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
// it. The middleware recorded the row the request's access token names; the
// session may have refreshed since (the request raced its own refresh), so
// when that row is not listed, the listed row further along its chain is the
// session's. rows are read before the chain: a rotation after that only
// extends the chain past the listed row, which stays on it.
func currentSessionRow(ctx context.Context, tx *gorm.DB, userID uint, rows []RefreshToken) (uint, error) {
	rid, ok := sessionFromContext(ctx)
	if !ok {
		return 0, nil
	}
	listed := make(map[uint]bool, len(rows))
	for _, row := range rows {
		if row.ID == rid {
			return rid, nil
		}
		listed[row.ID] = true
	}
	chain, err := sessionChain(tx, userID, rid)
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
// does not exist also yields ErrSessionNotFound.
func (s *Service) RevokeSession(ctx context.Context, userID uint, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		rows, err := s.activeSessionRows(tx, userID)
		if err != nil {
			return err
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
				return nil
			}
		}
		return errSessionNotFound
	})
}

// RevokeAllSessions ends every active session of the user. It returns
// ErrSessionNotFound when the user does not exist.
func (s *Service) RevokeAllSessions(ctx context.Context, userID uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		return revokeAllTx(tx, userID, s.now())
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

// sessionChain returns the ids of the user's refresh token rows from fromID
// along its session's chain: fromID, the row that replaced it, the row that
// replaced that one, and so on to the row nothing has replaced yet. A cycle
// (which rotation never writes) or an over-long chain stops the walk rather
// than looping.
func sessionChain(tx *gorm.DB, userID, fromID uint) ([]uint, error) {
	seen := map[uint]bool{fromID: true}
	ids := []uint{fromID}
	next := fromID
	for hop := 0; hop < maxChainHops; hop++ {
		var row RefreshToken
		err := tx.Where("id = ? AND user_id = ?", next, userID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		if row.ReplacedBy == nil || seen[*row.ReplacedBy] {
			break
		}
		next = *row.ReplacedBy
		seen[next] = true
		ids = append(ids, next)
	}
	return ids, nil
}

// revokeOtherSessions ends every active session of the user except the one
// whose chain includes the refresh token row keepID: the row the request's
// access token named, which may have rotated any number of times since the
// middleware checked it. With the user lock held no further rotation can
// start, so it keeps every row of the chain from keepID on, the session's
// current head included.
func (s *Service) revokeOtherSessions(ctx context.Context, userID, keepID uint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, userID); err != nil {
			return lockSessionsError(err)
		}
		keepIDs, err := sessionChain(tx, userID, keepID)
		if err != nil {
			return err
		}
		return tx.Model(&RefreshToken{}).
			Where("user_id = ? AND revoked_at IS NULL AND id NOT IN ?", userID, keepIDs).
			Update("revoked_at", s.now()).Error
	})
}
