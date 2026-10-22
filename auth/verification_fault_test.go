package auth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/internal/faulttest"
)

// Fault tests for a verification resend, which writes a pending token, mails
// it, then activates it and supersedes the earlier ones in one transaction.
// Whichever database write fails, at most one delivered link works, and an
// undelivered token never does.

// captureMailer records every message, delivered or not: a test can then try
// the token of a message that failed. fail, when set, fails Send.
type captureMailer struct {
	mu   sync.Mutex
	sent []auth.Message
	fail error
}

func (m *captureMailer) Send(_ context.Context, msg auth.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return m.fail
}

func (m *captureMailer) last() auth.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

func (m *captureMailer) setFail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// resendFixture is a registered user, their first link delivered, a minute
// later (the resend wait is over), over a faulted database.
type resendFixture struct {
	db    *database.DB
	svc   *auth.Service
	mail  *captureMailer
	clock *stepClock
	user  auth.User
	first string
}

func newResendFixture(t *testing.T, kind database.Driver, dsn string, faults *faulttest.DBFaults) resendFixture {
	t.Helper()
	faults.Disarm()
	db, err := faulttest.OpenDB(kind, dsn, faults)
	if err != nil {
		t.Fatal(err)
	}
	dropAuthTables := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m := db.WithContext(ctx).Migrator()
		_ = m.DropTable("auth_user_permissions", "auth_user_groups", "auth_group_permissions")
		_ = m.DropTable(auth.Models()...)
	}
	t.Cleanup(func() {
		faults.Disarm()
		dropAuthTables()
		_ = db.Close()
	})
	dropAuthTables()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	mail := &captureMailer{}
	svc, err := auth.NewService(db.DB, cfg, auth.WithMailer(mail))
	if err != nil {
		t.Fatal(err)
	}
	clock := &stepClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	auth.SetClock(svc, clock)
	user, err := svc.Register(context.Background(), fmt.Sprintf("resend-%d@example.com", time.Now().UnixNano()), testPassword)
	if err != nil {
		t.Fatal(err)
	}
	first := mail.last().Token
	clock.now = clock.now.Add(time.Minute)
	faults.Arm()
	return resendFixture{db: db, svc: svc, mail: mail, clock: clock, user: user, first: first}
}

// pendingTokens counts the user's undelivered tokens.
func (f resendFixture) pendingTokens(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.db.Model(&auth.OneTimeToken{}).Where("user_id = ? AND sent_at IS NULL", f.user.ID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// assertOnlyFirstWorks fails t unless the link mailed by the failed resend
// does not verify and the first one does.
func (f resendFixture) assertOnlyFirstWorks(t *testing.T) {
	t.Helper()
	faulttest.Idle(t, f.db)
	ctx := context.Background()
	if _, err := f.svc.VerifyEmail(ctx, f.mail.last().Token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("the link of the failed resend = %v, want ErrInvalidToken", err)
	}
	if user, err := f.svc.VerifyEmail(ctx, f.first); err != nil || !user.EmailVerified() {
		t.Fatalf("the first link after a failed resend = %v; it was not superseded, so it must work", err)
	}
}

// updatesTokenSentAt matches the activation: the UPDATE that sets a token's
// sent_at.
func updatesTokenSentAt(query string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "UPDATE") &&
		strings.Contains(query, "auth_one_time_tokens") && strings.Contains(query, "sent_at")
}

// deletesTokens matches the withdrawal of a pending token.
func deletesTokens(query string) bool {
	return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(query)), "DELETE") && strings.Contains(query, "auth_one_time_tokens")
}

// TestFault_Database_ResendActivationFails: the message was delivered, then
// activating its token fails. The resend reports the failure; the delivered
// link does not work, the earlier one still does, and the token is
// withdrawn, so the user can ask again at once.
func TestFault_Database_ResendActivationFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newResendFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailOnce(faulttest.ErrInjected),
			Match:     updatesTokenSentAt,
		})
		if err := f.svc.SendEmailVerification(context.Background(), f.user); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("SendEmailVerification = %v, want the injected fault", err)
		}
		if n := f.pendingTokens(t); n != 0 {
			t.Fatalf("%d pending tokens after the failed activation, want it withdrawn", n)
		}
		f.assertOnlyFirstWorks(t)
	})
}

// TestFault_Database_ResendActivationCommitFails: the activation's commit
// fails, so neither the activation nor the supersession happened.
func TestFault_Database_ResendActivationCommitFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		// Commit 1 writes the pending token; commit 2 is the activation.
		f := newResendFixture(t, kind, dsn, &faulttest.DBFaults{Commit: faulttest.FailOnCall(2, faulttest.ErrInjected)})
		if err := f.svc.SendEmailVerification(context.Background(), f.user); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("SendEmailVerification = %v, want the commit fault", err)
		}
		f.assertOnlyFirstWorks(t)
	})
}

// TestFault_Database_ResendWithdrawalFails: the message could not be sent,
// and withdrawing its token fails too. The token left behind is pending: it
// does not verify, and the earlier link still works. It counts as a message
// in flight until the interval has passed, then the user can ask again.
func TestFault_Database_ResendWithdrawalFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		f := newResendFixture(t, kind, dsn, &faulttest.DBFaults{
			Statement: faulttest.FailOnce(faulttest.ErrInjected),
			Match:     deletesTokens,
		})
		f.mail.setFail(errors.New("smtp down"))
		ctx := context.Background()
		if err := f.svc.SendEmailVerification(ctx, f.user); err == nil {
			t.Fatal("the resend reported success with a failing mailer")
		}
		f.mail.setFail(nil)
		if n := f.pendingTokens(t); n != 1 {
			t.Fatalf("%d pending tokens, want the one whose withdrawal failed", n)
		}
		if err := f.svc.SendEmailVerification(ctx, f.user); !auth.IsResendTooSoon(err) {
			t.Fatalf("a resend while the stale token is in flight = %v, want the resend wait", err)
		}
		f.clock.now = f.clock.now.Add(time.Minute)
		if err := f.svc.SendEmailVerification(ctx, f.user); err != nil {
			t.Fatalf("a resend once the interval passed: %v", err)
		}
		// The latest delivered link supersedes the first; the stale pending
		// token never works.
		if _, err := f.svc.VerifyEmail(ctx, f.first); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("the first link after a delivered resend = %v, want ErrInvalidToken", err)
		}
		if _, err := f.svc.VerifyEmail(ctx, f.mail.last().Token); err != nil {
			t.Fatalf("the latest delivered link: %v", err)
		}
	})
}

// TestFault_Database_PasswordResetRevocationFails: revoking the sessions
// fails after the new password was written, in the same transaction. The
// reset fails whole: the old password still works, the sessions stay, and
// the token is not used up.
func TestFault_Database_PasswordResetRevocationFails(t *testing.T) {
	faulttest.ForEachDB(t, faultDBs, func(t *testing.T, kind database.Driver, dsn string) {
		faults := &faulttest.DBFaults{Statement: faulttest.FailOnce(faulttest.ErrInjected), Match: updatesRefreshTokens}
		f := newResendFixture(t, kind, dsn, faults)
		faults.Disarm()
		ctx := context.Background()
		pair, err := f.svc.IssueTokens(ctx, f.user)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.svc.RequestPasswordReset(ctx, f.user.Email); err != nil {
			t.Fatal(err)
		}
		token := f.mail.last().Token
		faults.Arm()
		if _, err := f.svc.ResetPassword(ctx, token, "brand-new-horse"); !errors.Is(err, faulttest.ErrInjected) {
			t.Fatalf("ResetPassword = %v, want the injected fault", err)
		}
		faulttest.Idle(t, f.db)
		if _, err := f.svc.Authenticate(ctx, f.user.Email, testPassword); err != nil {
			t.Fatalf("the old password after a failed reset: %v", err)
		}
		if _, err := f.svc.RotateRefresh(ctx, pair.RefreshToken); err != nil {
			t.Fatalf("a session after a failed reset: %v", err)
		}
		if _, err := f.svc.ResetPassword(ctx, token, "brand-new-horse"); err != nil {
			t.Fatalf("the token after a failed reset: %v", err)
		}
	})
}
