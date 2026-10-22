package auth

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

// Mailer delivers the messages of the account lifecycle (AUTH-1): the email
// verification link, and later password reset and email change. It is the
// one seam to an email provider: the application implements it over its own
// (SMTP, an API) and passes it with WithMailer (or framework.WithAuthMailer).
// Tests use authtest.Mailer, which records the messages instead of sending
// them.
//
// Send is synchronous: it returns nil only once the provider accepted the
// message (the SMTP server took it, the API answered), because that is when
// the message's token starts working and the user's earlier ones stop. A
// Mailer that only enqueues the message would supersede the earlier links
// before the new one is delivered, and a delivery that then failed could
// not undo it. Send is called on the request that needs the message. An
// error is the message not going out: the request reports it where the
// message is the point (a resend), and logs it where it is a side effect
// (registration).
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// MessageKind names a lifecycle message, so a Mailer can pick its own
// template.
type MessageKind string

const (
	// MessageVerifyEmail asks the user to confirm they own the address.
	MessageVerifyEmail MessageKind = "verify_email"
	// MessageResetPassword carries a link to set a new password.
	MessageResetPassword MessageKind = "reset_password"
)

// Message is one lifecycle email. Subject and Text are a plain default a
// Mailer may send as they are; Token, URL and ExpiresAt let it render its
// own. Token is a credential: a Mailer must not log it.
type Message struct {
	Kind MessageKind
	// To is the address the message goes to.
	To string
	// Subject and Text are the default subject and plain-text body.
	Subject string
	Text    string
	// Token is the single-use token the message carries, and URL the link
	// with it (config.AuthConfig.EmailVerificationURL or PasswordResetURL;
	// empty when none is configured).
	Token string
	URL   string
	// ExpiresAt is when the token stops working.
	ExpiresAt time.Time
}

// ErrNoMailer is returned when a message must be sent and no Mailer is
// configured (WithMailer).
var ErrNoMailer = errors.New("auth: no mailer is configured, so the message cannot be sent")

type noMailer struct{}

func (noMailer) Send(context.Context, Message) error { return ErrNoMailer }

// LogMailer is a development Mailer: it logs each message, link and token
// included, instead of sending it, so a local app's verification flow works
// with no email provider. framework.New uses it outside production when no
// Mailer is set. Never use it in production: the log would hold credentials.
func LogMailer(logger *zap.Logger) Mailer {
	if logger == nil {
		logger = zap.NewNop()
	}
	return logMailer{logger: logger}
}

type logMailer struct{ logger *zap.Logger }

func (m logMailer) Send(_ context.Context, msg Message) error {
	m.logger.Info("auth: email not sent (development LogMailer)",
		zap.String("kind", string(msg.Kind)),
		zap.String("to", msg.To),
		zap.String("subject", msg.Subject),
		zap.String("url", msg.URL),
		zap.String("token", msg.Token),
		zap.Time("expires_at", msg.ExpiresAt),
	)
	return nil
}
