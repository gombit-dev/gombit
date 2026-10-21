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
// consumed by its first use. Sending a new one supersedes every earlier one
// for the user: only the latest link works. A user may ask for a new one
// once every verificationResendInterval.

// verificationResendInterval is how long a user waits between two
// verification messages.
const verificationResendInterval = time.Minute

var (
	errInvalidOneTimeToken = errors.New("auth: invalid or expired token")
	errAlreadyVerified     = errors.New("auth: email is already verified")
	errMailNotSent         = errors.New("auth: the message could not be sent")
)

// resendTooSoonError is a verification message asked for within
// verificationResendInterval of the previous one.
type resendTooSoonError struct{ retryAfter time.Duration }

func (e resendTooSoonError) Error() string {
	return fmt.Sprintf("auth: a verification message was sent less than %s ago", verificationResendInterval)
}

// ErrInvalidToken is returned by VerifyEmail for a token that is unknown,
// expired, already used, superseded by a newer one, or issued for an
// address the user no longer has. The cases are not told apart.
var ErrInvalidToken = errInvalidOneTimeToken

// ErrAlreadyVerified is returned by SendEmailVerification for a user whose
// email is verified.
var ErrAlreadyVerified = errAlreadyVerified

// SendEmailVerification issues a new verification token for user's email
// and mails it, superseding the user's earlier verification tokens. It
// returns ErrAlreadyVerified for a verified user, an error naming the wait
// when the previous message went out less than a minute ago, and the
// Mailer's error (ErrNoMailer when none is configured) when the message
// could not be sent; the new token is then withdrawn, so the user can ask
// again at once.
func (s *Service) SendEmailVerification(ctx context.Context, user User) error {
	var now time.Time
	var raw string
	var token OneTimeToken
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The user's row lock serializes two resends of one user (on SQLite,
		// which has no row locks, two at once can both pass the wait below:
		// both are sent, and the later supersedes the earlier). The clock is
		// read after it, so a resend that waited compares the instant it
		// runs at.
		locked, err := lockUserTx(tx, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		now = s.now()
		if locked.EmailVerified() {
			return errAlreadyVerified
		}
		var last OneTimeToken
		err = tx.Where("user_id = ? AND purpose = ?", locked.ID, PurposeVerifyEmail).Order("id DESC").Limit(1).Find(&last).Error
		if err != nil {
			return err
		}
		// Compared here, on instants: SQLite compares times as text.
		if last.ID != 0 && now.Sub(last.CreatedAt) < verificationResendInterval {
			return resendTooSoonError{retryAfter: verificationResendInterval - now.Sub(last.CreatedAt)}
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
	if err != nil {
		return err
	}
	if err := s.mailer.Send(ctx, s.verificationMessage(token, raw)); err != nil {
		// Not sent: withdraw it, so the wait does not apply to a message the
		// user never got, and leave the earlier links working.
		if derr := s.db.WithContext(context.WithoutCancel(ctx)).Delete(&OneTimeToken{}, token.ID).Error; derr != nil {
			s.logger.Warn("auth: withdraw an unsent verification token", zap.Error(derr))
		}
		if errors.Is(err, ErrNoMailer) {
			return err
		}
		return fmt.Errorf("%w: %w", errMailNotSent, err)
	}
	// Sent: it supersedes the earlier links. One statement on the token rows
	// alone, so it takes no lock VerifyEmail could hold in the other order.
	if err := s.db.WithContext(context.WithoutCancel(ctx)).Model(&OneTimeToken{}).
		Where("user_id = ? AND purpose = ? AND used_at IS NULL AND id <> ?", token.UserID, PurposeVerifyEmail, token.ID).
		Update("used_at", now).Error; err != nil {
		return err
	}
	return nil
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
// unknown, expired, used, superseded, or for an address the user no longer
// has. Concurrent uses of one token: exactly one succeeds.
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
		if token.UsedAt != nil || !now.Before(token.ExpiresAt) {
			return errInvalidOneTimeToken
		}
		// Consume it: the first use wins, a concurrent one finds it used.
		used := tx.Model(&OneTimeToken{}).Where("id = ? AND used_at IS NULL", token.ID).Update("used_at", now)
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
		if locked.EmailVerifiedAt == nil {
			if err := tx.Model(&User{}).Where("id = ?", locked.ID).Update("email_verified_at", now).Error; err != nil {
				return err
			}
			locked.EmailVerifiedAt = &now
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

	huma.Register(api, huma.Operation{
		OperationID: "auth-resend-email-verification",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/verify-email/resend",
		Summary:     "Send a new email verification message",
		Description: "Supersedes the earlier verification tokens: only the new one works. At most one message a minute (429 with Retry-After); 409 when the email is already verified.",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{security: {}}},
		Middlewares: huma.Middlewares{gate},
	}, svc.resendVerification)
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
