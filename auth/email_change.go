package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Email change (AUTH-4, #334). A signed-in user asks for a new address,
// giving their current password (POST /auth/email/change); a one-time
// token goes to the new address (onetime.go: pending until delivered,
// expiring after AuthConfig.EmailVerificationTTL, at most one message a
// minute); posting it (POST /auth/email/change/confirm) makes the new
// address the account's email, verified. Until then the account's email,
// and everything bound to it, is the old one: the new address is never
// trusted before it is proven. The old address is told once it changed.

var (
	errEmailUnchanged        = errors.New("auth: the new email is the account's email already")
	errInvalidEmail          = errors.New("auth: not an email address")
	errEmailChangedMeanwhile = errors.New("auth: the account's email changed during the request")
)

// ErrEmailUnchanged is RequestEmailChange's error for a new email that is
// the account's already.
var ErrEmailUnchanged = errEmailUnchanged

// ErrInvalidEmail is RequestEmailChange's error for a new email that is not
// an address.
var ErrInvalidEmail = errInvalidEmail

// ErrEmailChangedMeanwhile is RequestEmailChange's error when another change
// of the account's email committed while the request ran: the request was
// for the old one.
var ErrEmailChangedMeanwhile = errEmailChangedMeanwhile

// RequestEmailChange mails a confirmation link to newEmail, the address
// userID asks to change their email to, once current is their password. The
// account's email does not change here; ConfirmEmailChange does it, with the
// link. It returns ErrInvalidEmail or ErrEmailUnchanged for a new address
// that is no address or the account's own, ErrWrongCurrentPassword
// (audited), ErrEmailTaken when another account has the address,
// ErrEmailChangedMeanwhile when another change committed meanwhile, the
// resend wait (one message a minute) or the Mailer's error, and
// ErrUserNotFound for an unknown userID. A newer request supersedes the
// earlier link: only the latest delivered one works.
func (s *Service) RequestEmailChange(ctx context.Context, userID uint, current, newEmail string) error {
	newEmail = normalizeEmail(newEmail)
	if newEmail == "" || !strings.Contains(newEmail, "@") {
		return errInvalidEmail
	}
	var user User
	if err := s.db.WithContext(ctx).Where("id = ?", userID).Limit(1).Find(&user).Error; err != nil {
		return err
	}
	if user.ID == 0 {
		return errUserNotFound
	}
	if newEmail == user.Email {
		return errEmailUnchanged
	}
	if err := s.hasher.Compare(user.PasswordHash, current); err != nil {
		s.Audit(ctx, AuditEvent{Action: AuditEmailChangeFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "wrong_current_password")})
		return errWrongCurrentPassword
	}
	err := s.sendOneTimeToken(ctx, user, oneTimeFlow{
		purpose: PurposeChangeEmail,
		ttl:     s.verificationTTL(),
		to:      newEmail,
		check: func(tx *gorm.DB, locked User) error {
			// Under the user's lock: the request was for this email; another
			// change that committed meanwhile makes it stale.
			if locked.Email != user.Email {
				return errEmailChangedMeanwhile
			}
			var taken int64
			if err := tx.Model(&User{}).Where("email = ?", newEmail).Count(&taken).Error; err != nil {
				return err
			}
			if taken > 0 {
				return errEmailTaken
			}
			return nil
		},
		message: s.emailChangeMessage,
	})
	if err != nil {
		return err
	}
	s.Audit(ctx, AuditEvent{Action: AuditEmailChangeRequested, TargetUserID: uintPtr(user.ID)})
	return nil
}

// ConfirmEmailChange consumes an email change token and makes the address
// it was mailed to the user's email, verified (the token proves it). It
// works only while the user's email is still the one the change was asked
// from: once that changed (another change, an admin), the token is
// ErrInvalidToken, like an unknown, expired, used or superseded one. An
// address another account took meanwhile is ErrEmailTaken (first come,
// first served: the unique email decides), and changes nothing. The other
// email change links stop working; links bound to the old address
// (verification, password reset) stop too, since the email they were
// issued for is gone. The old address is told, best effort.
func (s *Service) ConfirmEmailChange(ctx context.Context, raw string) (User, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return User{}, errInvalidOneTimeToken
	}
	var user User
	var oldEmail string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		token, locked, now, err := s.consumeOneTimeToken(tx, raw, PurposeChangeEmail)
		if err != nil {
			return err
		}
		oldEmail = locked.Email
		err = tx.Model(&User{}).Where("id = ?", locked.ID).
			Updates(map[string]any{"email": token.Email, "email_verified_at": now, "verified_email": token.Email}).Error
		if database.IsUniqueViolation(err) {
			return errEmailTaken
		}
		if err != nil {
			return err
		}
		if err := voidTokensTx(tx, locked.ID, PurposeChangeEmail, token.ID, now); err != nil {
			return err
		}
		locked.Email, locked.EmailVerifiedAt, locked.VerifiedEmail = token.Email, &now, token.Email
		user = locked
		return nil
	})
	if err != nil {
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditEmailChanged, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID)})
	s.notifyEmailChanged(ctx, oldEmail)
	return user, nil
}

// notifyEmailChanged tells the old address its account's email changed: a
// security notification, sent best effort after the change committed (a
// failure is logged, never answered). It goes through the background runner
// so a slow Mailer does not hold up the confirmation; when the runner does
// not take it (busy, closing, its queue full), it is sent here instead,
// under the same deadline, rather than dropped. It names no new address:
// the old owner may not be the one who changed it.
func (s *Service) notifyEmailChanged(ctx context.Context, oldEmail string) {
	msg := Message{
		Kind:    MessageEmailChanged,
		To:      oldEmail,
		Subject: "Your account's email address was changed",
		Text:    "The email address of the account that used this address was changed. If you did not change it, reset your password and contact the site's support.\n",
	}
	send := func(ctx context.Context) {
		err := s.mailer.Send(ctx, msg)
		if err != nil && !errors.Is(err, ErrNoMailer) {
			s.logger.Warn("auth: send the email-changed notification", zap.Error(err))
		}
	}
	if err := s.background.submit(ctx, send); err != nil {
		ctx, cancel := s.detach(ctx, backgroundTaskTimeout)
		defer cancel()
		send(ctx)
	}
}

func (s *Service) emailChangeMessage(token OneTimeToken, raw string) Message {
	return linkMessage(token, raw, linkTemplate{
		kind:        MessageChangeEmail,
		subject:     "Confirm your new email address",
		url:         s.cfg.EmailChangeURL,
		placeholder: config.EmailChangeURLToken,
		withURL:     "Confirm that this address is your account's new email:",
		withRaw:     "Confirm that this address is your account's new email with this code:",
	})
}

type authEmailChangeBody struct {
	CurrentPassword string `json:"current_password" minLength:"1" maxLength:"72" doc:"The password the user has now"`
	NewEmail        string `json:"new_email" minLength:"1" maxLength:"255" format:"email" example:"ada@new.example.com" doc:"The address to change the account's email to"`
}

type emailChangeInput struct {
	Body authEmailChangeBody
}

// authEmailChangeRequested is named for its schema, like AuthSession.
type authEmailChangeRequested struct {
	Sent bool `json:"sent" example:"true" doc:"True when the confirmation link was mailed to the new address"`
}

type emailChangeOutput struct {
	Body contract.Data[authEmailChangeRequested]
}

type authEmailChangeConfirmBody struct {
	Token string `json:"token" minLength:"1" maxLength:"128" example:"9f2c..." doc:"The token from the confirmation message"`
}

type emailChangeConfirmInput struct {
	Body authEmailChangeConfirmBody
}

type emailChangeConfirmOutput struct {
	Body contract.Data[PublicUser]
}

func registerEmailChangeRoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context))) {
	changePath := prefix + "/auth/email/change"
	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID:   "auth-request-email-change",
		Method:        http.MethodPost,
		Path:          changePath,
		Summary:       "Ask to change the signed-in user's email",
		Description:   "Requires the current password (a wrong one is a 422 on current_password). Mails a confirmation link to the new address; the account's email changes only once it is confirmed. 409 when another account has the address; at most one message a minute (429 with Retry-After).",
		Tags:          []string{"Auth"},
		Security:      []map[string][]string{{security: {}}},
		Middlewares:   huma.Middlewares{gate},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusTooManyRequests},
	}), svc.requestEmailChange)
	declareRetryAfter(api, changePath, http.StatusTooManyRequests, "Seconds until another confirmation message may be requested")

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "auth-confirm-email-change",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/email/change/confirm",
		Summary:     "Confirm an email change with the token mailed to the new address",
		Description: "Consumes the token: it works once, and only while the account's email is the one the change was asked from. The new address becomes the account's email, verified. An unknown, expired, used or superseded token is a 422 on token; an address another account took meanwhile is a 409.",
		Tags:        []string{"Auth"},
	}), svc.confirmEmailChange)
}

func (s *Service) requestEmailChange(ctx context.Context, input *emailChangeInput) (*emailChangeOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.RequestEmailChange(ctx, user.ID, input.Body.CurrentPassword, input.Body.NewEmail); err != nil {
		return nil, mapEmailChangeError(ctx, err)
	}
	return &emailChangeOutput{Body: contract.Data[authEmailChangeRequested]{Data: authEmailChangeRequested{Sent: true}}}, nil
}

func (s *Service) confirmEmailChange(ctx context.Context, input *emailChangeConfirmInput) (*emailChangeConfirmOutput, error) {
	user, err := s.ConfirmEmailChange(ctx, input.Body.Token)
	if err != nil {
		return nil, mapEmailChangeError(ctx, err)
	}
	return &emailChangeConfirmOutput{Body: contract.Data[PublicUser]{Data: toPublicUser(user)}}, nil
}

// mapEmailChangeError reports the email change's own refusals, and the rest
// as mapServiceError does.
func mapEmailChangeError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errInvalidEmail):
		return invalidField(ctx, "new_email", "not an email address")
	case errors.Is(err, errEmailUnchanged):
		return invalidField(ctx, "new_email", "is the account's email already")
	case errors.Is(err, errEmailChangedMeanwhile):
		return contract.WithContext(ctx, contract.Conflict("the account's email changed meanwhile; try again"))
	}
	return mapServiceError(ctx, err)
}
