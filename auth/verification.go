package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Email verification (AUTH-1, #331). A token proves its holder received
// mail at the address it was issued for. Each is random (32 bytes), stored
// as its SHA-256, expires after AuthConfig.EmailVerificationTTL, and is
// consumed by its first use. It is pending until its message was delivered
// to the Mailer; delivery activates it and supersedes every earlier one for
// the user, in one transaction: only the latest delivered link works. A
// user may ask for a new one once every verificationResendInterval.

// verificationResendInterval is how long a user waits between two
// verification messages.
const verificationResendInterval = time.Minute

var (
	errInvalidOneTimeToken = errors.New("auth: invalid or expired token")
	errAlreadyVerified     = errors.New("auth: email is already verified")
	errMailNotSent         = errors.New("auth: the message could not be sent")
	errTokenNotPending     = errors.New("auth: the one-time token is no longer pending")
)

// resendTooSoonError is a verification message asked for within
// verificationResendInterval of the previous one.
type resendTooSoonError struct{ retryAfter time.Duration }

func (e resendTooSoonError) Error() string {
	return fmt.Sprintf("auth: a verification message was sent less than %s ago", verificationResendInterval)
}

// ErrInvalidToken is returned by VerifyEmail for a token that is unknown,
// expired, already used, superseded by a newer one, never delivered, or
// issued for an address the user no longer has. The cases are not told
// apart.
var ErrInvalidToken = errInvalidOneTimeToken

// ErrAlreadyVerified is returned by SendEmailVerification for a user whose
// email is verified.
var ErrAlreadyVerified = errAlreadyVerified

// SendEmailVerification issues a new verification token for user's email
// and mails it. It returns ErrAlreadyVerified for a verified user, an error
// naming the wait when the previous message went out (or is going out) less
// than a minute ago, and the Mailer's error (ErrNoMailer when none is
// configured) when the message could not be sent.
//
// The token is written pending, mailed, then activated: activating it and
// superseding the earlier tokens is one transaction. Whatever fails, the
// earlier links keep working unless the new one does instead. A message
// that could not be sent is withdrawn, so the user can ask again at once;
// if withdrawing it fails too, the pending token still does not work, and
// counts as a message in flight until the interval has passed.
func (s *Service) SendEmailVerification(ctx context.Context, user User) error {
	raw, token, err := s.issueVerificationToken(ctx, user)
	if err != nil {
		return err
	}
	if err := s.mailer.Send(ctx, s.verificationMessage(token, raw)); err != nil {
		s.withdrawToken(ctx, token)
		if errors.Is(err, ErrNoMailer) {
			return err
		}
		return fmt.Errorf("%w: %w", errMailNotSent, err)
	}
	if err := s.activateToken(ctx, token); err != nil {
		// Delivered, but its link does not work: withdraw it, so the user is
		// not made to wait for a message that is no use.
		s.withdrawToken(ctx, token)
		return err
	}
	return nil
}

// issueVerificationToken writes a pending verification token for user, once
// the resend wait allows one.
func (s *Service) issueVerificationToken(ctx context.Context, user User) (string, OneTimeToken, error) {
	var raw string
	var token OneTimeToken
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The user's row lock serializes two resends of one user: the second
		// finds the first's token (pending: in flight) and waits out the
		// interval (on SQLite, which has no row locks, two at once can both
		// pass: both are sent, and the later activated supersedes the
		// earlier). The clock is read after it, so a resend that waited
		// compares the instant it runs at.
		locked, err := lockUserTx(tx, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		now := s.now()
		if locked.EmailVerified() {
			return errAlreadyVerified
		}
		if wait, err := verificationWait(tx, locked.ID, now); err != nil {
			return err
		} else if wait > 0 {
			return resendTooSoonError{retryAfter: wait}
		}
		if raw, err = newOpaqueToken(); err != nil {
			return err
		}
		token = OneTimeToken{
			UserID:    locked.ID,
			Purpose:   PurposeVerifyEmail,
			TokenHash: hashOpaqueToken(raw),
			Email:     locked.Email,
			ExpiresAt: now.Add(s.verificationTTL()),
			CreatedAt: now,
		}
		return tx.Create(&token).Error
	})
	return raw, token, err
}

// verificationWait is how long the user must still wait before another
// verification message: the interval runs from the latest message
// delivered, or from the latest one still in flight (a pending token, which
// another request is mailing).
//
// "Latest delivered" is by activation, not by issuance: two sends can
// overlap and finish out of order, and the later activation is the link
// that works (activateToken), so it is the one the interval runs from.
// sent_at orders that in SQL on SQLite too, which stores times as text:
// activation writes it in UTC, so every value has the same layout and
// offset, and the text order is the time order.
func verificationWait(tx *gorm.DB, userID uint, now time.Time) (time.Duration, error) {
	var sent, pending OneTimeToken
	err := tx.Where("user_id = ? AND purpose = ? AND sent_at IS NOT NULL", userID, PurposeVerifyEmail).Order("sent_at DESC, id DESC").Limit(1).Find(&sent).Error
	if err != nil {
		return 0, err
	}
	err = tx.Where("user_id = ? AND purpose = ? AND sent_at IS NULL", userID, PurposeVerifyEmail).Order("id DESC").Limit(1).Find(&pending).Error
	if err != nil {
		return 0, err
	}
	// Compared here, on instants: SQLite compares times as text.
	var wait time.Duration
	if sent.SentAt != nil {
		wait = verificationResendInterval - now.Sub(*sent.SentAt)
	}
	if pending.ID != 0 {
		wait = max(wait, verificationResendInterval-now.Sub(pending.CreatedAt))
	}
	return max(wait, 0), nil
}

// activateToken makes a delivered token work and supersedes the user's
// earlier delivered tokens for its purpose, in one transaction: either the
// new link works and no earlier one does, or nothing changed. The user's
// row first, as VerifyEmail takes it, so the two never deadlock.
func (s *Service) activateToken(ctx context.Context, token OneTimeToken) error {
	return s.db.WithContext(context.WithoutCancel(ctx)).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, token.UserID); err != nil {
			return err
		}
		// UTC: verificationWait orders by sent_at.
		now := s.now().UTC()
		activated := tx.Model(&OneTimeToken{}).Where("id = ? AND sent_at IS NULL AND used_at IS NULL", token.ID).Update("sent_at", now)
		if activated.Error != nil {
			return activated.Error
		}
		if activated.RowsAffected != 1 {
			return errTokenNotPending
		}
		return tx.Model(&OneTimeToken{}).
			Where("user_id = ? AND purpose = ? AND sent_at IS NOT NULL AND used_at IS NULL AND id <> ?", token.UserID, token.Purpose, token.ID).
			Update("used_at", now).Error
	})
}

// withdrawToken deletes a pending token whose message is no use. Best
// effort: a pending token left behind does not work anyway.
func (s *Service) withdrawToken(ctx context.Context, token OneTimeToken) {
	if err := s.db.WithContext(context.WithoutCancel(ctx)).Where("id = ? AND sent_at IS NULL", token.ID).Delete(&OneTimeToken{}).Error; err != nil {
		s.logger.Warn("auth: withdraw an undelivered one-time token", zap.Error(err))
	}
}

func (s *Service) verificationTTL() time.Duration {
	if s.cfg.EmailVerificationTTL > 0 {
		return s.cfg.EmailVerificationTTL
	}
	return config.DefaultEmailVerificationTTL
}

func (s *Service) verificationMessage(token OneTimeToken, raw string) Message {
	msg := Message{
		Kind:      MessageVerifyEmail,
		To:        token.Email,
		Subject:   "Verify your email address",
		Token:     raw,
		ExpiresAt: token.ExpiresAt,
	}
	expires := token.ExpiresAt.UTC().Format(time.RFC1123)
	if tmpl := strings.TrimSpace(s.cfg.EmailVerificationURL); tmpl != "" {
		msg.URL = strings.ReplaceAll(tmpl, config.EmailVerificationURLToken, raw)
		msg.Text = fmt.Sprintf("Confirm that this is your email address:\n\n%s\n\nThe link works once, until %s. If you did not ask for it, ignore this message.\n", msg.URL, expires)
		return msg
	}
	msg.Text = fmt.Sprintf("Confirm that this is your email address with this code:\n\n%s\n\nIt works once, until %s. If you did not ask for it, ignore this message.\n", raw, expires)
	return msg
}

// VerifyEmail consumes a verification token and marks the email it was
// issued for verified. It returns ErrInvalidToken for a token that is
// unknown, expired, used, superseded, never delivered, or for an address the
// user no longer has. Concurrent uses of one token: exactly one succeeds.
func (s *Service) VerifyEmail(ctx context.Context, raw string) (User, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return User{}, errInvalidOneTimeToken
	}
	var user User
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token OneTimeToken
		err := tx.Where("token_hash = ? AND purpose = ?", hashOpaqueToken(raw), PurposeVerifyEmail).First(&token).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInvalidOneTimeToken
		}
		if err != nil {
			return err
		}
		// The user's row first, as SendEmailVerification takes it: the same
		// order, so the two never deadlock. Then the clock.
		locked, err := lockUserTx(tx, token.UserID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInvalidOneTimeToken
		}
		if err != nil {
			return err
		}
		now := s.now()
		// Expiry on instants, here: SQLite compares times as text.
		if token.SentAt == nil || token.UsedAt != nil || !now.Before(token.ExpiresAt) {
			return errInvalidOneTimeToken
		}
		// Consume it: the first use wins, a concurrent one finds it used.
		used := tx.Model(&OneTimeToken{}).Where("id = ? AND sent_at IS NOT NULL AND used_at IS NULL", token.ID).Update("used_at", now)
		if used.Error != nil {
			return used.Error
		}
		if used.RowsAffected != 1 {
			return errInvalidOneTimeToken
		}
		// The token proves the address it was sent to, not the account.
		if locked.Email != token.Email {
			return errInvalidOneTimeToken
		}
		// The verification is bound to the address: it lapses if Email
		// changes (User.EmailVerified).
		if !locked.EmailVerified() {
			if err := tx.Model(&User{}).Where("id = ?", locked.ID).
				Updates(map[string]any{"email_verified_at": now, "verified_email": token.Email}).Error; err != nil {
				return err
			}
			locked.EmailVerifiedAt, locked.VerifiedEmail = &now, token.Email
		}
		user = locked
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// sendVerificationOnRegister mails a new user their first verification
// link. Registration does not fail with it: the user exists, and can ask
// again (POST /auth/verify-email/resend).
func (s *Service) sendVerificationOnRegister(ctx context.Context, user User) {
	err := s.SendEmailVerification(ctx, user)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoMailer):
		// No mailer: the application does not verify emails. framework.New
		// warns about it once, at startup.
	default:
		s.logger.Warn("auth: send the verification message of a new user", zap.Uint("user_id", user.ID), zap.Error(err))
	}
}

type authVerifyEmailBody struct {
	Token string `json:"token" minLength:"1" maxLength:"128" example:"9f2c..." doc:"The token from the verification message"`
}

type verifyEmailInput struct {
	Body authVerifyEmailBody
}

type verifyEmailOutput struct {
	Body contract.Data[PublicUser]
}

// authVerificationResult is named for its schema, like AuthSession.
type authVerificationResult struct {
	Sent bool `json:"sent" example:"true" doc:"True when a new verification message was sent"`
}

type resendVerificationOutput struct {
	Body contract.Data[authVerificationResult]
}

func registerVerificationRoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context))) {
	huma.Register(api, huma.Operation{
		OperationID: "auth-verify-email",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/verify-email",
		Summary:     "Verify the user's email address with the token mailed to it",
		Description: "Consumes the token: it works once. An unknown, expired, used or superseded token, or one sent to an address the user no longer has, is a 422 on token.",
		Tags:        []string{"Auth"},
	}, svc.verifyEmail)

	resendPath := prefix + "/auth/verify-email/resend"
	huma.Register(api, huma.Operation{
		OperationID: "auth-resend-email-verification",
		Method:      http.MethodPost,
		Path:        resendPath,
		Summary:     "Send a new email verification message",
		Description: "Once delivered, the new token supersedes the earlier ones: only the latest works. At most one message a minute (429 with Retry-After); 409 when the email is already verified.",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{security: {}}},
		Middlewares: huma.Middlewares{gate},
		Errors:      []int{http.StatusTooManyRequests},
	}, svc.resendVerification)

	// Declaring a specific Huma error suppresses its automatic default error
	// response, so retain the catch-all response and describe the header the
	// rate-limited runtime response already sends.
	responses := api.OpenAPI().Paths[resendPath].Post.Responses
	rateLimited := responses["429"]
	minimum := 1.0
	rateLimited.Headers = map[string]*huma.Param{
		"Retry-After": {
			Description: "Seconds until another verification message may be requested",
			Required:    true,
			Schema: &huma.Schema{
				Type:    "integer",
				Minimum: &minimum,
			},
		},
	}
	responses["default"] = &huma.Response{
		Description: "Error",
		Content:     rateLimited.Content,
	}
}

func (s *Service) verifyEmail(ctx context.Context, input *verifyEmailInput) (*verifyEmailOutput, error) {
	user, err := s.VerifyEmail(ctx, input.Body.Token)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &verifyEmailOutput{Body: contract.Data[PublicUser]{Data: toPublicUser(user)}}, nil
}

func (s *Service) resendVerification(ctx context.Context, _ *struct{}) (*resendVerificationOutput, error) {
	user, ok := UserFromContext(ctx)
	if !ok {
		return nil, contract.WithContext(ctx, contract.Authentication("not authenticated"))
	}
	if err := s.SendEmailVerification(ctx, user); err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &resendVerificationOutput{Body: contract.Data[authVerificationResult]{Data: authVerificationResult{Sent: true}}}, nil
}

// RequireVerifiedEmail is a Huma middleware for an application's own
// operations that only users with a verified email may use. It runs after
// the authentication gate (Service.RequireBearer or RequireCookieSession),
// which stores the user it loaded on the request; it answers 403 when that
// user's email is unverified, and 401 when no gate authenticated the
// request.
//
//	Middlewares: huma.Middlewares{svc.RequireBearer(), auth.RequireVerifiedEmail()}
func RequireVerifiedEmail() func(ctx huma.Context, next func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		user, ok := UserFromContext(ctx.Context())
		switch {
		case !ok:
			writeAuthChallenge(ctx, contract.WithContext(ctx.Context(), contract.Authentication("not authenticated")), "")
		case !user.EmailVerified():
			writeAuthChallenge(ctx, contract.WithContext(ctx.Context(), contract.Authorization("email address not verified")), "")
		default:
			next(ctx)
		}
	}
}
