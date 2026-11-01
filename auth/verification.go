package auth

import (
	"context"
	"errors"
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
// user may ask for a new one once every resendInterval (onetime.go).

var errAlreadyVerified = errors.New("auth: email is already verified")

// ErrAlreadyVerified is returned by SendEmailVerification for a user whose
// email is verified.
var ErrAlreadyVerified = errAlreadyVerified

// SendEmailVerification issues a new verification token for user's email
// and mails it. It returns ErrAlreadyVerified for a verified user, an error
// naming the wait when the previous message went out (or is going out) less
// than a minute ago, and the Mailer's error (ErrNoMailer when none is
// configured) when the message could not be sent. The token is pending
// until the message was delivered, then supersedes the earlier ones
// (sendOneTimeToken).
func (s *Service) SendEmailVerification(ctx context.Context, user User) error {
	return s.sendOneTimeToken(ctx, user, oneTimeFlow{
		purpose: PurposeVerifyEmail,
		ttl:     s.verificationTTL(),
		check: func(_ *gorm.DB, u User) error {
			if u.EmailVerified() {
				return errAlreadyVerified
			}
			return nil
		},
		message: s.verificationMessage,
	})
}

func (s *Service) verificationTTL() time.Duration {
	if s.cfg.EmailVerificationTTL > 0 {
		return s.cfg.EmailVerificationTTL
	}
	return config.DefaultEmailVerificationTTL
}

func (s *Service) verificationMessage(token OneTimeToken, raw string) Message {
	return linkMessage(token, raw, linkTemplate{
		kind:        MessageVerifyEmail,
		subject:     "Verify your email address",
		url:         s.cfg.EmailVerificationURL,
		placeholder: config.EmailVerificationURLToken,
		withURL:     "Confirm that this is your email address:",
		withRaw:     "Confirm that this is your email address with this code:",
	})
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
		token, locked, now, err := s.consumeOneTimeToken(tx, raw, PurposeVerifyEmail)
		if err != nil {
			return err
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
	s.Audit(ctx, AuditEvent{Action: AuditEmailVerified, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID)})
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
	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "auth-verify-email",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/verify-email",
		Summary:     "Verify the user's email address with the token mailed to it",
		Description: "Consumes the token: it works once. An unknown, expired, used or superseded token, or one sent to an address the user no longer has, is a 422 on token.",
		Tags:        []string{"Auth"},
	}), svc.verifyEmail)

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
	declareRetryAfter(api, resendPath, http.StatusTooManyRequests, "Seconds until another verification message may be requested")
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
