package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// One-time tokens: the mailed, single-use tokens of the account lifecycle
// (email verification, AUTH-1; password reset, AUTH-2). Each is random (32
// bytes), stored as its SHA-256 in auth_one_time_tokens, expires after its
// purpose's TTL, and is consumed by its first use. It is pending until its
// message was delivered to the Mailer; delivery activates it and supersedes
// the user's earlier tokens for the purpose, in one transaction: only the
// latest delivered link works. A user may ask for a new message of a
// purpose once every resendInterval.

// resendInterval is how long a user waits between two messages of one
// purpose.
const resendInterval = time.Minute

var (
	errInvalidOneTimeToken = errors.New("auth: invalid or expired token")
	errMailNotSent         = errors.New("auth: the message could not be sent")
	errTokenNotPending     = errors.New("auth: the one-time token is no longer pending")
)

// resendTooSoonError is a message asked for within resendInterval of the
// previous one of its purpose.
type resendTooSoonError struct{ retryAfter time.Duration }

func (e resendTooSoonError) Error() string {
	return fmt.Sprintf("auth: a message was sent less than %s ago", resendInterval)
}

// ErrInvalidToken is returned by VerifyEmail and ResetPassword for a token
// that is unknown, expired, already used, superseded by a newer one, never
// delivered, or issued for an address the user no longer has. The cases are
// not told apart.
var ErrInvalidToken = errInvalidOneTimeToken

// oneTimeFlow is what a purpose plugs into sendOneTimeToken.
type oneTimeFlow struct {
	purpose TokenPurpose
	ttl     time.Duration
	// check refuses the message for the locked user (nil: always sent).
	check   func(User) error
	message func(token OneTimeToken, raw string) Message
}

// sendOneTimeToken issues a token of f's purpose for user's email and
// mails it. The token is written pending, mailed, then activated:
// activating it and superseding the earlier tokens is one transaction.
// Whatever fails, the earlier links keep working unless the new one does
// instead. A message that could not be sent is withdrawn, so the user can
// ask again at once; if withdrawing it fails too, the pending token still
// does not work, and counts as a message in flight until the interval has
// passed.
func (s *Service) sendOneTimeToken(ctx context.Context, user User, f oneTimeFlow) error {
	raw, token, err := s.issueOneTimeToken(ctx, user, f)
	if err != nil {
		return err
	}
	if err := s.mailer.Send(ctx, f.message(token, raw)); err != nil {
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

// issueOneTimeToken writes a pending token of f's purpose for user, once
// f.check and the resend wait allow one.
func (s *Service) issueOneTimeToken(ctx context.Context, user User, f oneTimeFlow) (string, OneTimeToken, error) {
	var raw string
	var token OneTimeToken
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The user's row lock serializes two sends of one user: the second
		// finds the first's token (pending: in flight) and waits out the
		// interval (on SQLite, which has no row locks, two at once can both
		// pass: both are sent, and the later activated supersedes the
		// earlier). The clock is read after it, so a send that waited
		// compares the instant it runs at.
		locked, err := lockUserTx(tx, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		now := s.now()
		if f.check != nil {
			if err := f.check(locked); err != nil {
				return err
			}
		}
		if wait, err := oneTimeWait(tx, locked.ID, f.purpose, now); err != nil {
			return err
		} else if wait > 0 {
			return resendTooSoonError{retryAfter: wait}
		}
		if raw, err = newOpaqueToken(); err != nil {
			return err
		}
		token = OneTimeToken{
			UserID:    locked.ID,
			Purpose:   f.purpose,
			TokenHash: hashOpaqueToken(raw),
			Email:     locked.Email,
			ExpiresAt: now.Add(f.ttl),
			CreatedAt: now,
		}
		return tx.Create(&token).Error
	})
	return raw, token, err
}

// oneTimeWait is how long the user must still wait before another message
// of purpose: the interval runs from the latest message delivered, or from
// the latest one still in flight (a pending token, which another request is
// mailing).
//
// "Latest delivered" is by activation, not by issuance: two sends can
// overlap and finish out of order, and the later activation is the link
// that works (activateToken), so it is the one the interval runs from.
// sent_at orders that in SQL on SQLite too, which stores times as text:
// activation writes it in UTC, so every value has the same layout and
// offset, and the text order is the time order.
func oneTimeWait(tx *gorm.DB, userID uint, purpose TokenPurpose, now time.Time) (time.Duration, error) {
	var sent, pending OneTimeToken
	err := tx.Where("user_id = ? AND purpose = ? AND sent_at IS NOT NULL", userID, purpose).Order("sent_at DESC, id DESC").Limit(1).Find(&sent).Error
	if err != nil {
		return 0, err
	}
	err = tx.Where("user_id = ? AND purpose = ? AND sent_at IS NULL", userID, purpose).Order("id DESC").Limit(1).Find(&pending).Error
	if err != nil {
		return 0, err
	}
	// Compared here, on instants: SQLite compares times as text.
	var wait time.Duration
	if sent.SentAt != nil {
		wait = resendInterval - now.Sub(*sent.SentAt)
	}
	if pending.ID != 0 {
		wait = max(wait, resendInterval-now.Sub(pending.CreatedAt))
	}
	return max(wait, 0), nil
}

// activateToken makes a delivered token work and supersedes the user's
// earlier delivered tokens for its purpose, in one transaction: either the
// new link works and no earlier one does, or nothing changed. The user's
// row first, as consumeOneTimeToken takes it, so the two never deadlock.
func (s *Service) activateToken(ctx context.Context, token OneTimeToken) error {
	return s.db.WithContext(context.WithoutCancel(ctx)).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, token.UserID); err != nil {
			return err
		}
		// UTC: oneTimeWait orders by sent_at.
		now := s.now().UTC()
		activated := tx.Model(&OneTimeToken{}).Where("id = ? AND sent_at IS NULL AND used_at IS NULL", token.ID).Update("sent_at", now)
		if activated.Error != nil {
			return activated.Error
		}
		if activated.RowsAffected != 1 {
			return errTokenNotPending
		}
		return supersedeTokensTx(tx, token.UserID, token.Purpose, token.ID, now)
	})
}

// supersedeTokensTx marks the user's delivered, unused tokens of purpose
// used, but keep.
func supersedeTokensTx(tx *gorm.DB, userID uint, purpose TokenPurpose, keep uint, now time.Time) error {
	return tx.Model(&OneTimeToken{}).
		Where("user_id = ? AND purpose = ? AND sent_at IS NOT NULL AND used_at IS NULL AND id <> ?", userID, purpose, keep).
		Update("used_at", now).Error
}

// withdrawToken deletes a pending token whose message is no use. Best
// effort: a pending token left behind does not work anyway.
func (s *Service) withdrawToken(ctx context.Context, token OneTimeToken) {
	if err := s.db.WithContext(context.WithoutCancel(ctx)).Where("id = ? AND sent_at IS NULL", token.ID).Delete(&OneTimeToken{}).Error; err != nil {
		s.logger.Warn("auth: withdraw an undelivered one-time token", zap.Error(err))
	}
}

// consumeOneTimeToken uses up the token raw of purpose, in tx: it returns
// the token, its user (the row locked), and the instant it was used. It
// returns errInvalidOneTimeToken for a token that is unknown, never
// delivered, used, expired, or issued for an address the user no longer
// has. Concurrent uses of one token: exactly one succeeds.
func (s *Service) consumeOneTimeToken(tx *gorm.DB, raw string, purpose TokenPurpose) (OneTimeToken, User, time.Time, error) {
	var token OneTimeToken
	err := tx.Where("token_hash = ? AND purpose = ?", hashOpaqueToken(raw), purpose).First(&token).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OneTimeToken{}, User{}, time.Time{}, errInvalidOneTimeToken
	}
	if err != nil {
		return OneTimeToken{}, User{}, time.Time{}, err
	}
	// The user's row first, as sendOneTimeToken takes it: the same order, so
	// the two never deadlock. Then the clock.
	locked, err := lockUserTx(tx, token.UserID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OneTimeToken{}, User{}, time.Time{}, errInvalidOneTimeToken
	}
	if err != nil {
		return OneTimeToken{}, User{}, time.Time{}, err
	}
	now := s.now()
	// Expiry on instants, here: SQLite compares times as text.
	if token.SentAt == nil || token.UsedAt != nil || !now.Before(token.ExpiresAt) {
		return OneTimeToken{}, User{}, time.Time{}, errInvalidOneTimeToken
	}
	// Consume it: the first use wins, a concurrent one finds it used.
	used := tx.Model(&OneTimeToken{}).Where("id = ? AND sent_at IS NOT NULL AND used_at IS NULL", token.ID).Update("used_at", now)
	if used.Error != nil {
		return OneTimeToken{}, User{}, time.Time{}, used.Error
	}
	if used.RowsAffected != 1 {
		return OneTimeToken{}, User{}, time.Time{}, errInvalidOneTimeToken
	}
	// The token proves the address it was sent to, not the account.
	if locked.Email != token.Email {
		return OneTimeToken{}, User{}, time.Time{}, errInvalidOneTimeToken
	}
	return token, locked, now, nil
}

// linkTemplate is the plain default text of a lifecycle message.
type linkTemplate struct {
	kind    MessageKind
	subject string
	// url is the configured link, with placeholder where the token goes;
	// empty sends the token alone.
	url, placeholder string
	// withURL and withRaw introduce the link, or the bare token.
	withURL, withRaw string
}

// linkMessage is the message carrying token (raw) as t describes.
func linkMessage(token OneTimeToken, raw string, t linkTemplate) Message {
	msg := Message{
		Kind:      t.kind,
		To:        token.Email,
		Subject:   t.subject,
		Token:     raw,
		ExpiresAt: token.ExpiresAt,
	}
	expires := token.ExpiresAt.UTC().Format(time.RFC1123)
	if tmpl := strings.TrimSpace(t.url); tmpl != "" {
		msg.URL = strings.ReplaceAll(tmpl, t.placeholder, raw)
		msg.Text = fmt.Sprintf("%s\n\n%s\n\nThe link works once, until %s. If you did not ask for it, ignore this message.\n", t.withURL, msg.URL, expires)
		return msg
	}
	msg.Text = fmt.Sprintf("%s\n\n%s\n\nIt works once, until %s. If you did not ask for it, ignore this message.\n", t.withRaw, raw, expires)
	return msg
}
