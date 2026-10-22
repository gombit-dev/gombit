package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Password reset (AUTH-2, #332). POST /auth/password/forgot mails a
// single-use reset token (onetime.go: pending until delivered, expires
// after AuthConfig.PasswordResetTTL, at most one message a minute) to an
// account's address, and answers the same whether or not the address is an
// account. POST /auth/password/reset consumes the token, sets the new
// password, and by default revokes every session of the user.

// RequestPasswordReset mails a password reset token to the account whose
// email is email. For an address that is no account it does nothing and
// returns nil. Otherwise it returns what sending returns: an error naming
// the wait when the previous reset message went out (or is going out) less
// than a minute ago, or the Mailer's error. The result tells whether email
// is an account: an endpoint must not show it (POST /auth/password/forgot
// answers before it is known).
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	email = normalizeEmail(email)
	if email == "" {
		return nil
	}
	var user User
	err := s.db.WithContext(ctx).Where("email = ?", email).Limit(1).Find(&user).Error
	if err != nil {
		return err
	}
	if user.ID == 0 {
		return nil
	}
	err = s.sendOneTimeToken(ctx, user, oneTimeFlow{
		purpose: PurposeResetPassword,
		ttl:     s.passwordResetTTL(),
		message: s.passwordResetMessage,
	})
	if err != nil {
		return err
	}
	s.Audit(ctx, AuditEvent{Action: AuditPasswordResetRequested, TargetUserID: uintPtr(user.ID)})
	return nil
}

// ResetPassword consumes a password reset token and sets the user's
// password to password, hashed as registration hashes it (the Hasher's
// limits apply). It returns ErrInvalidToken for a token that is unknown,
// expired, used, superseded, never delivered, or issued for an address the
// user no longer has. The user's other reset tokens stop working, and
// unless AuthConfig.PasswordResetKeepSessions, so do all of the user's
// sessions and refresh tokens, in the same transaction.
func (s *Service) ResetPassword(ctx context.Context, raw, password string) (User, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return User{}, errInvalidOneTimeToken
	}
	// A token that cannot work is refused before bcrypt runs, so a stream
	// of made-up (or permanently dead) tokens costs two indexed lookups
	// each, not a hash, and a database that cannot answer fails the request
	// before any hashing. The transaction below decides; this only screens.
	if ok, err := s.resetTokenMayWork(ctx, raw); err != nil {
		return User{}, err
	} else if !ok {
		return User{}, errInvalidOneTimeToken
	}
	// Hashed before the transaction: bcrypt is slow, and must not run under
	// the user's lock.
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return User{}, err
	}
	revoke := !s.cfg.PasswordResetKeepSessions
	var user User
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		token, locked, now, err := s.consumeOneTimeToken(tx, raw, PurposeResetPassword)
		if err != nil {
			return err
		}
		if err := tx.Model(&User{}).Where("id = ?", locked.ID).Update("password_hash", hash).Error; err != nil {
			return err
		}
		// Every other reset token of the user stops working, the pending ones
		// too: a message still being sent must not activate a link to the
		// password just replaced (activateToken needs used_at NULL).
		if err := tx.Model(&OneTimeToken{}).
			Where("user_id = ? AND purpose = ? AND used_at IS NULL AND id <> ?", locked.ID, PurposeResetPassword, token.ID).
			Update("used_at", now).Error; err != nil {
			return err
		}
		if revoke {
			if err := revokeAllTx(tx, locked.ID, now); err != nil {
				return err
			}
		}
		locked.PasswordHash = hash
		user = locked
		return nil
	})
	if err != nil {
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditPasswordReset, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID), Details: details("sessions_revoked", strconv.FormatBool(revoke))})
	return user, nil
}

// resetTokenMayWork reports whether raw names a delivered, unused,
// unexpired reset token issued for the address its user has now: what
// consumeOneTimeToken will accept, short of a race. A database error is
// returned, not taken for a yes.
func (s *Service) resetTokenMayWork(ctx context.Context, raw string) (bool, error) {
	db := s.db.WithContext(ctx)
	var token OneTimeToken
	if err := db.Where("token_hash = ? AND purpose = ?", hashOpaqueToken(raw), PurposeResetPassword).Limit(1).Find(&token).Error; err != nil {
		return false, err
	}
	// Expiry on instants, here: SQLite compares times as text.
	if token.ID == 0 || token.SentAt == nil || token.UsedAt != nil || !s.now().Before(token.ExpiresAt) {
		return false, nil
	}
	var user User
	if err := db.Select("id", "email").Where("id = ?", token.UserID).Limit(1).Find(&user).Error; err != nil {
		return false, err
	}
	return user.ID != 0 && user.Email == token.Email, nil
}

// requestPasswordResetInBackground hands RequestPasswordReset to the
// Service's background runner (background.go), so the answer is the same,
// and as fast, for an account and for an address that is none. The task
// keeps the request's values (its id, client address, for the audit event)
// but runs under the runner's deadline and shutdown; its outcome is logged,
// never answered. It returns errBackgroundBusy when the runner did not admit
// it (a full queue, or a closed Service): decided before the lookup, so it
// is the same for any address.
func (s *Service) requestPasswordResetInBackground(ctx context.Context, email string) error {
	return s.background.submit(ctx, func(ctx context.Context) {
		err := s.RequestPasswordReset(ctx, email)
		var tooSoon resendTooSoonError
		switch {
		case err == nil, errors.As(err, &tooSoon):
		case errors.Is(err, ErrNoMailer):
			// framework.New warned about it at startup.
		default:
			s.logger.Warn("auth: send a password reset message", zap.Error(err))
		}
	})
}

func (s *Service) passwordResetTTL() time.Duration {
	if s.cfg.PasswordResetTTL > 0 {
		return s.cfg.PasswordResetTTL
	}
	return config.DefaultPasswordResetTTL
}

func (s *Service) passwordResetMessage(token OneTimeToken, raw string) Message {
	return linkMessage(token, raw, linkTemplate{
		kind:        MessageResetPassword,
		subject:     "Reset your password",
		url:         s.cfg.PasswordResetURL,
		placeholder: config.PasswordResetURLToken,
		withURL:     "Set a new password for your account:",
		withRaw:     "Set a new password for your account with this code:",
	})
}

type authPasswordForgotBody struct {
	Email string `json:"email" minLength:"1" maxLength:"255" format:"email" example:"ada@example.com" doc:"The account's email"`
}

type passwordForgotInput struct {
	Body authPasswordForgotBody
}

// authPasswordResetRequested is named for its schema, like AuthSession.
type authPasswordResetRequested struct {
	Accepted bool `json:"accepted" example:"true" doc:"Always true: the answer does not say whether the email is an account"`
}

type passwordForgotOutput struct {
	Body contract.Data[authPasswordResetRequested]
}

type authPasswordResetBody struct {
	Token    string `json:"token" minLength:"1" maxLength:"128" example:"9f2c..." doc:"The token from the password reset message"`
	Password string `json:"password" minLength:"8" maxLength:"72" example:"correct-horse" doc:"The new password"`
}

type passwordResetInput struct {
	Body authPasswordResetBody
}

// authPasswordResetResult is named for its schema, like AuthSession.
type authPasswordResetResult struct {
	OK bool `json:"ok" example:"true"`
	// SessionsRevoked reports whether the reset signed the user out
	// everywhere (AuthConfig.PasswordResetKeepSessions false).
	SessionsRevoked bool `json:"sessions_revoked" example:"true" doc:"True when every session of the user was revoked: sign in again with the new password"`
}

type passwordResetOutput struct {
	Body contract.Data[authPasswordResetResult]
}

type cookiePasswordResetOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[authPasswordResetResult]
}

func registerPasswordResetRoutes(api huma.API, svc *Service, prefix string, cookieMode bool) {
	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID:   "auth-request-password-reset",
		Method:        http.MethodPost,
		Path:          prefix + "/auth/password/forgot",
		Summary:       "Mail a password reset link to an account",
		Description:   "202 with the same body whether or not the email is an account, answered before that is known: the message is sent in the background. 503 (Retry-After) when the server cannot take more background work at once, also before the lookup. At most one message a minute per account; the link works once, until it expires.",
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusAccepted,
	}), svc.forgotPassword)

	reset := withRequestInfo(huma.Operation{
		OperationID: "auth-reset-password",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/password/reset",
		Summary:     "Set a new password with the token from a reset message",
		Description: "Consumes the token: it works once. Revokes every session of the user unless the app keeps them. An unknown, expired, used or superseded token, or one sent to an address the user no longer has, is a 422 on token.",
		Tags:        []string{"Auth"},
	})
	if cookieMode {
		huma.Register(api, reset, svc.resetPasswordCookie)
		return
	}
	huma.Register(api, reset, svc.resetPassword)
}

func (s *Service) forgotPassword(ctx context.Context, input *passwordForgotInput) (*passwordForgotOutput, error) {
	if err := s.requestPasswordResetInBackground(ctx, input.Body.Email); err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &passwordForgotOutput{Body: contract.Data[authPasswordResetRequested]{Data: authPasswordResetRequested{Accepted: true}}}, nil
}

func (s *Service) resetPassword(ctx context.Context, input *passwordResetInput) (*passwordResetOutput, error) {
	if _, err := s.ResetPassword(ctx, input.Body.Token, input.Body.Password); err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &passwordResetOutput{Body: contract.Data[authPasswordResetResult]{Data: s.passwordResetResult()}}, nil
}

// resetPasswordCookie also clears the browser's session cookies when the
// reset revoked the sessions: the session they name has ended. With the
// sessions kept, the browser keeps its own.
func (s *Service) resetPasswordCookie(ctx context.Context, input *passwordResetInput) (*cookiePasswordResetOutput, error) {
	if _, err := s.ResetPassword(ctx, input.Body.Token, input.Body.Password); err != nil {
		return nil, mapServiceError(ctx, err)
	}
	out := &cookiePasswordResetOutput{Body: contract.Data[authPasswordResetResult]{Data: s.passwordResetResult()}}
	if !s.cfg.PasswordResetKeepSessions {
		out.SetCookie = s.expiredSessionCookies()
	}
	return out, nil
}

func (s *Service) passwordResetResult() authPasswordResetResult {
	return authPasswordResetResult{OK: true, SessionsRevoked: !s.cfg.PasswordResetKeepSessions}
}
