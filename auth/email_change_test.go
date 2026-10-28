package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

func TestEmailChangeService(t *testing.T) {
	runEmailChangeService(t, openSQLite(t))
}

// changeToken asks for user's email to change to newEmail and returns the
// token mailed to newEmail.
func (e verificationEnv) changeToken(t *testing.T, user auth.User, newEmail string) string {
	t.Helper()
	if err := e.svc.RequestEmailChange(context.Background(), user.ID, testPassword, newEmail); err != nil {
		t.Fatalf("RequestEmailChange(%s): %v", newEmail, err)
	}
	msg, ok := e.mail.Last(newEmail)
	if !ok || msg.Kind != auth.MessageChangeEmail {
		t.Fatalf("no change_email message to %s (last: %+v)", newEmail, msg)
	}
	return msg.Token
}

func (e verificationEnv) reload(t *testing.T, id uint) auth.User {
	t.Helper()
	var user auth.User
	if err := e.db.First(&user, id).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func runEmailChangeService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()
	fresh := func(name string) string { return fmt.Sprintf("%s-%d@example.com", name, time.Now().UnixNano()) }

	t.Run("the new address is trusted only once confirmed", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, verify := e.register(t, "change-email")
		if _, err := e.svc.VerifyEmail(ctx, verify); err != nil {
			t.Fatal(err)
		}
		newEmail := fresh("new-address")
		token := e.changeToken(t, user, newEmail)
		// Asked, not confirmed: the account is still the old address.
		if got := e.reload(t, user.ID); got.Email != user.Email || !got.EmailVerified() {
			t.Fatalf("before confirmation the account is %q (verified %v), want the old, verified address", got.Email, got.EmailVerified())
		}
		if _, err := e.svc.Authenticate(ctx, newEmail, testPassword); err == nil {
			t.Fatal("the unconfirmed address signs in")
		}
		changed, err := e.svc.ConfirmEmailChange(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if changed.Email != newEmail || !changed.EmailVerified() {
			t.Fatalf("after confirmation = %q verified %v", changed.Email, changed.EmailVerified())
		}
		if _, err := e.svc.Authenticate(ctx, newEmail, testPassword); err != nil {
			t.Fatalf("signing in with the new address: %v", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err == nil {
			t.Fatal("the old address still signs in")
		}
		// The old address is told, without a token or the new address, in
		// the background: Close waits for it.
		if err := e.svc.Close(ctx); err != nil {
			t.Fatal(err)
		}
		note, ok := e.mail.Last(user.Email)
		if !ok || note.Kind != auth.MessageEmailChanged || note.Token != "" || strings.Contains(note.Text, newEmail) {
			t.Fatalf("the old address's notification = %+v", note)
		}
		if _, err := e.svc.ConfirmEmailChange(ctx, token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("the token again = %v, want ErrInvalidToken", err)
		}
		var events []auth.AuditEvent
		e.db.Where("target_user_id = ? AND action LIKE ?", user.ID, "auth.email.change%").Order("id").Find(&events)
		var actions []auth.AuditAction
		for _, ev := range events {
			actions = append(actions, ev.Action)
			if strings.Contains(ev.Details, "@") {
				t.Fatalf("an audit row records an address: %+v", ev)
			}
		}
		if fmt.Sprint(actions) != fmt.Sprint([]auth.AuditAction{auth.AuditEmailChangeRequested, auth.AuditEmailChanged}) {
			t.Fatalf("audit = %v", actions)
		}
	})

	t.Run("the notification is best effort", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-notice")
		// A Mailer failing the notification fails nothing.
		auth.SetMailer(e.svc, failKind{inner: e.mail, kind: auth.MessageEmailChanged})
		if _, err := e.svc.ConfirmEmailChange(ctx, e.changeToken(t, user, fresh("notice-fails"))); err != nil {
			t.Fatalf("confirming with a failing notification = %v", err)
		}
		if err := e.svc.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// A runner that takes no work (its bucket empty): sent with the
		// confirmation instead of dropped.
		busy := newVerificationEnv(t, db)
		stopped := time.Now()
		auth.SetBackgroundAdmission(busy.svc, 0, 1e-9, 1, func() time.Time { return stopped })
		other, _ := busy.register(t, "change-email-notice-busy")
		if _, err := busy.svc.ConfirmEmailChange(ctx, busy.changeToken(t, other, fresh("notice-inline"))); err != nil {
			t.Fatal(err)
		}
		if note, ok := busy.mail.Last(other.Email); !ok || note.Kind != auth.MessageEmailChanged {
			t.Fatalf("with the runner busy the notification = %+v (sent %v)", note, ok)
		}
	})

	t.Run("a request racing another change is refused", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-race")
		// Another change commits between the request's read of the account
		// and its lock (here: while the password is compared).
		moved := fresh("moved")
		auth.SetHasher(e.svc, hookHasher{Hasher: auth.HasherOf(e.svc), before: func() {
			if err := e.db.Model(&auth.User{}).Where("id = ?", user.ID).Update("email", moved).Error; err != nil {
				t.Error(err)
			}
		}})
		newEmail := fresh("racing")
		if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, newEmail); !errors.Is(err, auth.ErrEmailChangedMeanwhile) {
			t.Fatalf("a request racing another change = %v, want ErrEmailChangedMeanwhile", err)
		}
		if _, ok := e.mail.Last(newEmail); ok {
			t.Fatal("a link went out for a stale request")
		}
	})

	t.Run("a request racing a password change is refused", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-reset-race")
		// A reset commits after the request checked the old password, before
		// its lock: the old password must not authorize anything after it.
		auth.SetHasher(e.svc, hookHasher{Hasher: auth.HasherOf(e.svc), before: func() {
			if err := e.db.Model(&auth.User{}).Where("id = ?", user.ID).Update("password_hash", "$2a$04$replacedbyaresetmeanwhile....................").Error; err != nil {
				t.Error(err)
			}
		}})
		newEmail := fresh("reset-race")
		if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, newEmail); !errors.Is(err, auth.ErrPasswordChanged) {
			t.Fatalf("a request racing a reset = %v, want ErrPasswordChanged", err)
		}
		if _, ok := e.mail.Last(newEmail); ok {
			t.Fatal("a link went out on the old password")
		}
	})

	t.Run("a password reset or change voids the change links", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-recovered")
		// Asked with the password a change replaces: the link dies with it.
		changed := e.changeToken(t, user, fresh("before-change"))
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, "a-changed-horse", auth.PasswordChange{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ConfirmEmailChange(ctx, changed); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a change link after a password change = %v, want ErrInvalidToken", err)
		}
		// Asked before a reset (the account recovered from whoever asked):
		// the link dies with the reset.
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		target := fresh("before-reset")
		if err := e.svc.RequestEmailChange(ctx, user.ID, "a-changed-horse", target); err != nil {
			t.Fatal(err)
		}
		msg, _ := e.mail.Last(target)
		if _, err := e.svc.ResetPassword(ctx, e.resetToken(t, user), "a-recovered-horse"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ConfirmEmailChange(ctx, msg.Token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a change link after a reset = %v, want ErrInvalidToken", err)
		}
		if got := e.reload(t, user.ID); got.Email != user.Email {
			t.Fatalf("the account's email = %q, want %q", got.Email, user.Email)
		}
	})

	t.Run("links stay stopped when the email changes back", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, verify := e.register(t, "change-email-back")
		reset := e.resetToken(t, user)
		if _, err := e.svc.ConfirmEmailChange(ctx, e.changeToken(t, user, fresh("away"))); err != nil {
			t.Fatal(err)
		}
		// A -> B -> A: the account's email is A again.
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, user.Email); err != nil {
			t.Fatal(err)
		}
		back, ok := e.mail.Last(user.Email)
		if !ok || back.Kind != auth.MessageChangeEmail {
			t.Fatalf("no change link back to %s (last %+v)", user.Email, back)
		}
		if _, err := e.svc.ConfirmEmailChange(ctx, back.Token); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ResetPassword(ctx, reset, "a-revived-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reset link from before A -> B -> A = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.VerifyEmail(ctx, verify); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a verification link from before A -> B -> A = %v, want ErrInvalidToken", err)
		}
		var open int64
		e.db.Model(&auth.OneTimeToken{}).Where("user_id = ? AND used_at IS NULL", user.ID).Count(&open)
		if open != 0 {
			t.Fatalf("unused tokens after the change back = %d, want 0", open)
		}
	})

	t.Run("the current password is required", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-wrong")
		newEmail := fresh("never")
		if err := e.svc.RequestEmailChange(ctx, user.ID, "not-my-password", newEmail); !errors.Is(err, auth.ErrWrongCurrentPassword) {
			t.Fatalf("a wrong password = %v", err)
		}
		if _, ok := e.mail.Last(newEmail); ok {
			t.Fatal("a link went out for a wrong password")
		}
	})

	t.Run("refused: no address, the same address", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-bad")
		// The service checks what the route's schema does: a bare address
		// that fits the column, for a Go caller too.
		for _, bad := range []string{"not-an-address", "@", "ada@", "@example.com", "ada@b@example.com", "ada lovelace@example.com", "Ada <ada@example.com>", strings.Repeat("a", 244) + "@example.com"} {
			if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, bad); !errors.Is(err, auth.ErrInvalidEmail) {
				t.Fatalf("RequestEmailChange(%q) = %v, want ErrInvalidEmail", bad, err)
			}
		}
		if len(e.mail.Messages()) != 1 {
			t.Fatalf("messages after refused requests = %d, want the registration's only", len(e.mail.Messages()))
		}
		if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, strings.ToUpper(user.Email)); !errors.Is(err, auth.ErrEmailUnchanged) {
			t.Fatalf("the same address = %v", err)
		}
	})

	t.Run("another account's address is refused, asked or confirmed", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-taken")
		other, _ := e.register(t, "change-email-other")
		if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, other.Email); !errors.Is(err, auth.ErrEmailTaken) {
			t.Fatalf("asking for another account's address = %v, want ErrEmailTaken", err)
		}
		// Free when asked, taken (another registration) before it is
		// confirmed: the unique email decides, and nothing changes.
		target := fresh("contested")
		token := e.changeToken(t, user, target)
		if _, err := e.svc.Register(ctx, target, testPassword); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ConfirmEmailChange(ctx, token); !errors.Is(err, auth.ErrEmailTaken) {
			t.Fatalf("confirming an address taken meanwhile = %v, want ErrEmailTaken", err)
		}
		if got := e.reload(t, user.ID); got.Email != user.Email {
			t.Fatalf("the account's email after a refused confirmation = %q", got.Email)
		}
	})

	t.Run("a link stops working once the email changed another way", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-stale")
		token := e.changeToken(t, user, fresh("stale"))
		if err := e.db.Model(&auth.User{}).Where("id = ?", user.ID).Update("email", "moved-"+user.Email).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ConfirmEmailChange(ctx, token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a link asked from an address the account no longer has = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("a newer request supersedes the earlier link", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-email-twice")
		first := e.changeToken(t, user, fresh("first"))
		if err := e.svc.RequestEmailChange(ctx, user.ID, testPassword, fresh("too-soon")); !auth.IsResendTooSoon(err) {
			t.Fatalf("a second request within a minute = %v, want the resend wait", err)
		}
		e.clock.now = e.clock.now.Add(time.Minute)
		second := fresh("second")
		secondToken := e.changeToken(t, user, second)
		if _, err := e.svc.ConfirmEmailChange(ctx, first); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("the superseded link = %v, want ErrInvalidToken", err)
		}
		if got, err := e.svc.ConfirmEmailChange(ctx, secondToken); err != nil || got.Email != second {
			t.Fatalf("the latest link = %+v, %v", got, err)
		}
	})

	t.Run("links bound to the old address stop working", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, verify := e.register(t, "change-email-old-links")
		reset := e.resetToken(t, user)
		token := e.changeToken(t, user, fresh("old-links"))
		if _, err := e.svc.ConfirmEmailChange(ctx, token); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.VerifyEmail(ctx, verify); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a verification link to the old address = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.ResetPassword(ctx, reset, "brand-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reset link to the old address = %v, want ErrInvalidToken", err)
		}
	})
}

// failKind fails the messages of one kind and hands the rest to inner.
type failKind struct {
	inner auth.Mailer
	kind  auth.MessageKind
}

func (m failKind) Send(ctx context.Context, msg auth.Message) error {
	if msg.Kind == m.kind {
		return errors.New("mailer down")
	}
	return m.inner.Send(ctx, msg)
}

// hookHasher runs before ahead of each Compare.
type hookHasher struct {
	auth.Hasher
	before func()
}

func (h hookHasher) Compare(hash, password string) error {
	h.before()
	return h.Hasher.Compare(hash, password)
}

func TestEmailChangeBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runEmailChangeBearer(t, openSQLite(t))
}

// runEmailChangeBearer: the request route needs a session and the current
// password; the confirm route is public (the link may open signed out).
func runEmailChangeBearer(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeJWT, mail, config.EnvironmentTest)
	email := fmt.Sprintf("change-email-http-%d@example.com", time.Now().UnixNano())
	newEmail := "new-" + email
	registerUser(t, app, email, testPassword)
	pair := loginUser(t, app, email, testPassword)
	post := func(path, access, body string) *httptest.ResponseRecorder { return bearerPost(app, path, access, body) }

	assertError(t, bearerPost(app, "/api/v1/auth/email/change", "", `{"current_password":"`+testPassword+`","new_email":"`+newEmail+`"}`), http.StatusUnauthorized, "authentication")
	wrong := post("/api/v1/auth/email/change", pair.AccessToken, `{"current_password":"not-mine","new_email":"`+newEmail+`"}`)
	if wrong.Code != http.StatusUnprocessableEntity || !strings.Contains(wrong.Body.String(), "current_password") {
		t.Fatalf("a wrong current password = %d %s", wrong.Code, wrong.Body.String())
	}
	otherEmail := "other-" + email
	registerUser(t, app, otherEmail, testPassword)
	assertError(t, post("/api/v1/auth/email/change", pair.AccessToken, `{"current_password":"`+testPassword+`","new_email":"`+otherEmail+`"}`), http.StatusConflict, "conflict")
	if rec := post("/api/v1/auth/email/change", pair.AccessToken, `{"current_password":"`+testPassword+`","new_email":"`+newEmail+`"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("request = %d %s", rec.Code, rec.Body.String())
	}
	tooSoon := post("/api/v1/auth/email/change", pair.AccessToken, `{"current_password":"`+testPassword+`","new_email":"other-`+newEmail+`"}`)
	if tooSoon.Code != http.StatusTooManyRequests || tooSoon.Header().Get("Retry-After") == "" {
		t.Fatalf("a second request within a minute = %d (Retry-After %q)", tooSoon.Code, tooSoon.Header().Get("Retry-After"))
	}
	msg, ok := mail.Last(newEmail)
	if !ok {
		t.Fatal("no confirmation to the new address")
	}
	rec := post("/api/v1/auth/email/change/confirm", "", `{"token":"`+msg.Token+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	var me dataBody[struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.Data.Email != newEmail || !me.Data.EmailVerified {
		t.Fatalf("confirm body = %s", rec.Body.String())
	}
	assertError(t, post("/api/v1/auth/email/change/confirm", "", `{"token":"`+msg.Token+`"}`), http.StatusUnprocessableEntity, "validation_error")
	loginUser(t, app, newEmail, testPassword)
}

func TestEmailChangeCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runEmailChangeCookie(t, openSQLite(t))
}

// runEmailChangeCookie: the same routes in cookie mode, behind CSRF.
func runEmailChangeCookie(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeCookie, mail, config.EnvironmentTest)
	email := fmt.Sprintf("change-email-cookie-%d@example.com", time.Now().UnixNano())
	newEmail := "new-" + email
	jar := fetchCSRF(t, app)
	registerCookieUser(t, app, jar, email, testPassword)
	if rec := loginCookieUser(t, app, jar, email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	body := `{"current_password":"` + testPassword + `","new_email":"` + newEmail + `"}`
	if rec := doRequest(app, nil, http.MethodPost, "/api/v1/auth/email/change", body); rec.Code != http.StatusForbidden {
		t.Fatalf("request without the CSRF header = %d, want 403", rec.Code)
	}
	if rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/email/change", body); rec.Code != http.StatusAccepted {
		t.Fatalf("request = %d %s", rec.Code, rec.Body.String())
	}
	msg, _ := mail.Last(newEmail)
	if rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/email/change/confirm", `{"token":"`+msg.Token+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeMe(t, doRequest(app, jar, http.MethodGet, "/api/v1/me", "")); got.Email != newEmail || !got.EmailVerified {
		t.Fatalf("/me after the change = %+v", got)
	}
}
