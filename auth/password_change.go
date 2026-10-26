package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
	"gorm.io/gorm"
)

// Password change (AUTH-3, #333): a signed-in user sets a new password by
// giving their current one. POST /auth/password/change.

// errWrongCurrentPassword is a password change whose current password did
// not match.
var errWrongCurrentPassword = errors.New("auth: the current password is incorrect")

// PasswordChange is what ChangePassword does besides setting the password.
type PasswordChange struct {
	// KeepOtherSessions leaves the user's other sessions signed in. By
	// default every session but Session ends: whoever knew the old password
	// is signed out.
	KeepOtherSessions bool
	// Session is the refresh token row of the session asking (the request's
	// own: it stays signed in). Zero is no session: then, unless
	// KeepOtherSessions, every session of the user ends.
	Session uint
}

// ChangePassword sets userID's password to newPassword once current is the
// password the user has now. The current password is checked first, and
// bcrypt runs outside any lock; then, under the user's row lock, the stored
// hash must still be the one checked (a reset or another change committed
// meanwhile is errPasswordChanged, a 409), the new hash is written, every
// outstanding password reset token of the user stops working (a reset link
// mailed before the change must not undo it), and, unless
// change.KeepOtherSessions, the user's other sessions end, all in one
// transaction. It returns whether other sessions were revoked. A wrong
// current password is errWrongCurrentPassword (a 422 on current_password),
// and is audited, as the change is.
func (s *Service) ChangePassword(ctx context.Context, userID uint, current, newPassword string, change PasswordChange) (bool, error) {
	var user User
	if err := s.db.WithContext(ctx).Where("id = ?", userID).Limit(1).Find(&user).Error; err != nil {
		return false, err
	}
	if user.ID == 0 {
		return false, errUserNotFound
	}
	// The policy first: refusing a new password costs no bcrypt.
	if err := checkNewPassword(newPassword, "new_password"); err != nil {
		return false, err
	}
	if err := s.hasher.Compare(user.PasswordHash, current); err != nil {
		s.Audit(ctx, AuditEvent{Action: AuditPasswordChangeFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "wrong_current_password")})
		return false, errWrongCurrentPassword
	}
	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return false, err
	}
	if s.onPasswordChangeVerified != nil {
		s.onPasswordChangeVerified()
	}
	revoke := !change.KeepOtherSessions
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if locked.PasswordHash != user.PasswordHash {
			return errPasswordChanged
		}
		now := s.now()
		// The requesting session must still be signed in, whatever happens
		// to the others: one that ended while this waited for the lock (a
		// revocation, its expiry) changes nothing (errSessionEnded). Checked
		// before any credential is written.
		var keep []uint
		if change.Session != 0 {
			if keep, err = requesterChain(tx, user.ID, change.Session, now); err != nil {
				return err
			}
		}
		if err := tx.Model(&User{}).Where("id = ?", user.ID).Update("password_hash", hash).Error; err != nil {
			return err
		}
		if err := voidResetTokensTx(tx, user.ID, 0, now); err != nil {
			return err
		}
		switch {
		case !revoke:
			return nil
		case len(keep) > 0:
			return revokeSessionsButTx(tx, user.ID, keep, now)
		default:
			return revokeAllTx(tx, user.ID, now)
		}
	})
	if err != nil {
		return false, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditPasswordChanged, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID), Details: details("other_sessions_revoked", strconv.FormatBool(revoke))})
	return revoke, nil
}

type authPasswordChangeBody struct {
	CurrentPassword   string `json:"current_password" minLength:"1" maxLength:"72" doc:"The password the user has now"`
	NewPassword       string `json:"new_password" minLength:"8" maxLength:"72" example:"correct-horse-2" doc:"The new password"`
	KeepOtherSessions bool   `json:"keep_other_sessions,omitempty" doc:"Keep the user's other sessions signed in (default: they are signed out)"`
}

type passwordChangeInput struct {
	Body authPasswordChangeBody
}

// authPasswordChangeResult is named for its schema, like AuthSession.
type authPasswordChangeResult struct {
	OK                   bool `json:"ok" example:"true"`
	OtherSessionsRevoked bool `json:"other_sessions_revoked" example:"true" doc:"True when every other session of the user was signed out; this one stays"`
}

type passwordChangeOutput struct {
	Body contract.Data[authPasswordChangeResult]
}

func registerPasswordChangeRoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context))) {
	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "auth-change-password",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/password/change",
		Summary:     "Change the signed-in user's password",
		Description: "Requires the current password (a wrong one is a 422 on current_password). Signs out every other session of the user unless keep_other_sessions; this session stays. Outstanding password reset links stop working.",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{security: {}}},
		Middlewares: huma.Middlewares{gate},
	}), svc.changePassword)
}

func (s *Service) changePassword(ctx context.Context, input *passwordChangeInput) (*passwordChangeOutput, error) {
	user, refreshID, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	revoked, err := s.ChangePassword(ctx, user.ID, input.Body.CurrentPassword, input.Body.NewPassword, PasswordChange{
		KeepOtherSessions: input.Body.KeepOtherSessions,
		Session:           refreshID,
	})
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &passwordChangeOutput{Body: contract.Data[authPasswordChangeResult]{Data: authPasswordChangeResult{OK: true, OtherSessionsRevoked: revoked}}}, nil
}
