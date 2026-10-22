package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

// countingHasher is a Hasher that counts Hash calls (cheap, for tests).
type countingHasher struct{ hashes atomic.Int64 }

func (h *countingHasher) Hash(password string) (string, error) {
	h.hashes.Add(1)
	return "hash:" + password, nil
}

func (h *countingHasher) Compare(hash, password string) error {
	if hash != "hash:"+password {
		return errors.New("mismatch")
	}
	return nil
}

// ctxMailer is a Mailer that sends nothing and returns only when its
// context is done, reporting why on returned.
type ctxMailer struct {
	once     sync.Once
	entered  chan struct{}
	returned chan error
}

func newCtxMailer() *ctxMailer {
	return &ctxMailer{entered: make(chan struct{}), returned: make(chan error, 1)}
}

func (m *ctxMailer) Send(ctx context.Context, _ auth.Message) error {
	m.once.Do(func() { close(m.entered) })
	<-ctx.Done()
	m.returned <- ctx.Err()
	return ctx.Err()
}

// awaitMail waits for a message of kind to address: POST
// /auth/password/forgot sends in the background.
func awaitMail(t *testing.T, mail *authtest.Mailer, address string, kind auth.MessageKind) auth.Message {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msg, ok := mail.Last(address); ok && msg.Kind == kind {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s message to %s", kind, address)
	return auth.Message{}
}

// awaitResetLink waits for the reset message to address and for its token
// to work: the background send activates the token only after the Mailer
// returned, so the message can be seen a moment before its link works.
func awaitResetLink(t *testing.T, db *database.DB, mail *authtest.Mailer, address string) auth.Message {
	t.Helper()
	msg := awaitMail(t, mail, address, auth.MessageResetPassword)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pending int64
		err := db.Model(&auth.OneTimeToken{}).
			Where("email = ? AND purpose = ? AND sent_at IS NULL", address, auth.PurposeResetPassword).Count(&pending).Error
		if err == nil && pending == 0 {
			return msg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the reset token to %s was not activated", address)
	return auth.Message{}
}

// forgotRequest is POST /auth/password/forgot, with the headers a browser
// would send.
func forgotRequest(app *framework.App, jar *cookieJar, email string) *httptest.ResponseRecorder {
	if jar != nil {
		return doRequest(app, jar, http.MethodPost, "/api/v1/auth/password/forgot", `{"email":"`+email+`"}`)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/forgot", strings.NewReader(`{"email":"`+email+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(framework.RequestIDHeader, "req-forgot")
	app.Router().ServeHTTP(rec, req)
	return rec
}

// assertIndistinguishable fails t unless a and b have the same status,
// body, and headers (aside from Date and the per-request ids).
func assertIndistinguishable(t *testing.T, a, b *httptest.ResponseRecorder) {
	t.Helper()
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("account: %d %s\nno account: %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	headers := func(rec *httptest.ResponseRecorder) map[string]string {
		out := map[string]string{}
		for k, v := range rec.Header() {
			if k == "Date" || k == framework.RequestIDHeader || k == "X-Trace-Id" {
				continue
			}
			out[k] = strings.Join(v, ",")
		}
		return out
	}
	ha, hb := headers(a), headers(b)
	if fmt.Sprint(ha) != fmt.Sprint(hb) {
		t.Fatalf("headers differ:\naccount:    %v\nno account: %v", ha, hb)
	}
}

func TestPasswordResetBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runPasswordResetBearer(t, openSQLite(t))
}

// runPasswordResetBearer: forgot answers the same for an account and for an
// address that is none; the mailed token sets a new password once, and the
// reset signs the user out everywhere.
func runPasswordResetBearer(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeJWT, mail, config.EnvironmentTest)
	email := fmt.Sprintf("reset-%d@example.com", time.Now().UnixNano())
	registerUser(t, app, email, testPassword)
	pair := loginUser(t, app, email, testPassword)

	known := forgotRequest(app, nil, email)
	unknown := forgotRequest(app, nil, "nobody-"+email)
	if known.Code != http.StatusAccepted {
		t.Fatalf("forgot = %d %s, want 202", known.Code, known.Body.String())
	}
	assertIndistinguishable(t, known, unknown)
	msg := awaitResetLink(t, db, mail, email)
	if _, ok := mail.Last("nobody-" + email); ok {
		t.Fatal("a message went to an address that is no account")
	}

	assertError(t, postJSON(t, app, "/api/v1/auth/password/reset", `{"token":"`+msg.Token+`","password":"short"}`), http.StatusUnprocessableEntity, "validation_error")
	const newPassword = "brand-new-horse"
	rec := postJSON(t, app, "/api/v1/auth/password/reset", `{"token":"`+msg.Token+`","password":"`+newPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	var result dataBody[struct {
		OK              bool `json:"ok"`
		SessionsRevoked bool `json:"sessions_revoked"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.Data.OK || !result.Data.SessionsRevoked {
		t.Fatalf("reset body = %s", rec.Body.String())
	}
	// Used: the same token again fails.
	reused := postJSON(t, app, "/api/v1/auth/password/reset", `{"token":"`+msg.Token+`","password":"another-new-horse"}`)
	assertError(t, reused, http.StatusUnprocessableEntity, "validation_error")
	// Every session ended: the old access and refresh tokens are dead.
	me := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	app.Router().ServeHTTP(me, req)
	if me.Code != http.StatusUnauthorized {
		t.Fatalf("/me with a pre-reset access token = %d, want 401", me.Code)
	}
	assertError(t, postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+pair.RefreshToken+`"}`), http.StatusUnauthorized, "authentication")
	assertError(t, postJSON(t, app, "/api/v1/auth/login", `{"email":"`+email+`","password":"`+testPassword+`"}`), http.StatusUnauthorized, "authentication")
	loginUser(t, app, email, newPassword)
}

func TestPasswordResetCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runPasswordResetCookie(t, openSQLite(t))
}

// runPasswordResetCookie is the cookie-mode surface: the same routes, CSRF
// on both, and the reset clears the browser's session cookies.
func runPasswordResetCookie(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeCookie, mail, config.EnvironmentTest)
	email := fmt.Sprintf("cookie-reset-%d@example.com", time.Now().UnixNano())
	jar := fetchCSRF(t, app)
	registerCookieUser(t, app, jar, email, testPassword)
	if rec := loginCookieUser(t, app, jar, email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, nil, http.MethodPost, "/api/v1/auth/password/forgot", `{"email":"`+email+`"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("forgot without the CSRF header = %d, want 403", rec.Code)
	}
	known := forgotRequest(app, jar, email)
	unknown := forgotRequest(app, jar, "nobody-"+email)
	assertIndistinguishable(t, known, unknown)
	msg := awaitResetLink(t, db, mail, email)

	rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/password/reset", `{"token":"`+msg.Token+`","password":"brand-new-horse"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	cleared := 0
	for _, c := range rec.Result().Cookies() {
		if (c.Name == auth.AccessCookieName || c.Name == auth.RefreshCookieName) && c.MaxAge < 0 {
			cleared++
		}
	}
	if cleared != 2 {
		t.Fatalf("the reset cleared %d session cookies, want both", cleared)
	}
	if rec := doRequest(app, jar, http.MethodGet, "/api/v1/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/me after the reset = %d, want 401", rec.Code)
	}
	if rec := loginCookieUser(t, app, jar, email, "brand-new-horse"); rec.Code != http.StatusOK {
		t.Fatalf("login with the new password = %d", rec.Code)
	}
}

// TestPasswordResetCookieKeepsSessions: with the sessions kept, a cookie
// reset leaves the browser's session cookies, and its session, alone.
func TestPasswordResetCookieKeepsSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeCookie, mail, config.EnvironmentTest, func(c *config.AuthConfig) { c.PasswordResetKeepSessions = true })
	email := fmt.Sprintf("cookie-keep-%d@example.com", time.Now().UnixNano())
	jar := fetchCSRF(t, app)
	registerCookieUser(t, app, jar, email, testPassword)
	if rec := loginCookieUser(t, app, jar, email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	forgotRequest(app, jar, email)
	msg := awaitResetLink(t, db, mail, email)
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/password/reset", `{"token":"`+msg.Token+`","password":"brand-new-horse"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessions_revoked":false`) {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.AccessCookieName || c.Name == auth.RefreshCookieName {
			t.Fatalf("a reset that kept the sessions set %s", c.Name)
		}
	}
	if rec := doRequest(app, jar, http.MethodGet, "/api/v1/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("/me after a reset that kept the sessions = %d, want 200", rec.Code)
	}
}

func TestPasswordResetService(t *testing.T) {
	runPasswordResetService(t, openSQLite(t))
}

// resetToken requests a reset for user and returns the mailed token.
func (e verificationEnv) resetToken(t *testing.T, user auth.User) string {
	t.Helper()
	if err := e.svc.RequestPasswordReset(context.Background(), user.Email); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	msg, ok := e.mail.Last(user.Email)
	if !ok || msg.Kind != auth.MessageResetPassword {
		t.Fatalf("no reset message to %s (last: %+v)", user.Email, msg)
	}
	return msg.Token
}

func runPasswordResetService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()

	t.Run("an address that is no account: nothing", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		e.mail.Reset()
		if err := e.svc.RequestPasswordReset(ctx, fmt.Sprintf("ghost-%d@example.com", time.Now().UnixNano())); err != nil {
			t.Fatalf("RequestPasswordReset(unknown) = %v, want nil", err)
		}
		if n := len(e.mail.Messages()); n != 0 {
			t.Fatalf("%d messages for an address that is no account", n)
		}
	})

	t.Run("sets the password once, revokes the sessions", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-svc")
		pair, err := e.svc.IssueTokens(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		token := e.resetToken(t, user)
		if _, err := e.svc.ResetPassword(ctx, token, "brand-new-horse"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, "brand-new-horse"); err != nil {
			t.Fatalf("the new password: %v", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err == nil {
			t.Fatal("the old password still works")
		}
		if _, err := e.svc.ResetPassword(ctx, token, "another-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reused token = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); err == nil {
			t.Fatal("a pre-reset refresh token still rotates")
		}
		var events []auth.AuditEvent
		e.db.Where("target_user_id = ? AND action LIKE ?", user.ID, "auth.password.%").Order("id").Find(&events)
		if len(events) != 2 || events[0].Action != auth.AuditPasswordResetRequested || events[1].Action != auth.AuditPasswordReset ||
			!strings.Contains(events[1].Details, `"sessions_revoked":"true"`) {
			t.Fatalf("audit events = %+v", events)
		}
	})

	t.Run("keeps the sessions when the app says so", func(t *testing.T) {
		cfg := config.DefaultFor(config.EnvironmentTest)
		cfg.Auth.JWTSecret = testJWTSecret
		cfg.Auth.BcryptCost = bcrypt.MinCost
		cfg.Auth.PasswordResetKeepSessions = true
		mail := &authtest.Mailer{}
		svc, err := auth.NewService(db.DB, cfg, auth.WithMailer(mail))
		if err != nil {
			t.Fatal(err)
		}
		user, err := svc.Register(ctx, fmt.Sprintf("keep-%d@example.com", time.Now().UnixNano()), testPassword)
		if err != nil {
			t.Fatal(err)
		}
		pair, _ := svc.IssueTokens(ctx, user)
		if err := svc.RequestPasswordReset(ctx, user.Email); err != nil {
			t.Fatal(err)
		}
		msg, _ := mail.Last(user.Email)
		if _, err := svc.ResetPassword(ctx, msg.Token, "brand-new-horse"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RotateRefresh(ctx, pair.RefreshToken); err != nil {
			t.Fatalf("a kept session: %v", err)
		}
	})

	t.Run("expires", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-expiry")
		token := e.resetToken(t, user)
		e.clock.now = e.clock.now.Add(config.DefaultPasswordResetTTL)
		if _, err := e.svc.ResetPassword(ctx, token, "brand-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("an expired token = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("at most one message a minute, superseding the earlier", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		// Registration mailed a verification message: a reset is another
		// purpose, with its own wait.
		user, _ := e.register(t, "reset-resend")
		first := e.resetToken(t, user)
		if err := e.svc.RequestPasswordReset(ctx, user.Email); !auth.IsResendTooSoon(err) {
			t.Fatalf("a second reset within a minute = %v, want the resend wait", err)
		}
		e.clock.now = e.clock.now.Add(time.Minute)
		second := e.resetToken(t, user)
		if _, err := e.svc.ResetPassword(ctx, first, "brand-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a superseded token = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.ResetPassword(ctx, second, "brand-new-horse"); err != nil {
			t.Fatalf("the latest token: %v", err)
		}
	})

	t.Run("purposes do not mix", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, verify := e.register(t, "reset-purpose")
		if _, err := e.svc.ResetPassword(ctx, verify, "brand-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a verification token as a reset token = %v, want ErrInvalidToken", err)
		}
		reset := e.resetToken(t, user)
		if _, err := e.svc.VerifyEmail(ctx, reset); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reset token as a verification token = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("bound to the address", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-moved")
		token := e.resetToken(t, user)
		if err := e.db.Model(&auth.User{}).Where("id = ?", user.ID).Update("email", "moved-"+user.Email).Error; err != nil {
			t.Fatal(err)
		}
		// Refused before bcrypt, every time: the token can never work.
		hasher := &countingHasher{}
		auth.SetHasher(e.svc, hasher)
		for range 3 {
			if _, err := e.svc.ResetPassword(ctx, token, "brand-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("a token for the old address = %v, want ErrInvalidToken", err)
			}
		}
		if n := hasher.hashes.Load(); n != 0 {
			t.Fatalf("bcrypt ran %d times for a token bound to an address the user no longer has", n)
		}
	})

	t.Run("the password policy applies", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-policy")
		token := e.resetToken(t, user)
		if _, err := e.svc.ResetPassword(ctx, token, strings.Repeat("x", 73)); err == nil {
			t.Fatal("a password over bcrypt's 72 bytes was accepted")
		}
		// Refused before the token was consumed: it still works.
		if _, err := e.svc.ResetPassword(ctx, token, "brand-new-horse"); err != nil {
			t.Fatalf("the token after a refused password: %v", err)
		}
	})

	t.Run("a full background refuses at once, and admits again when free", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-full")
		release := auth.FillBackground(e.svc)
		if err := auth.RequestPasswordResetInBackground(ctx, e.svc, user.Email); !auth.IsBackgroundBusy(err) {
			release()
			t.Fatalf("a request with no room = %v, want it refused (a 503), not dropped behind a 202", err)
		}
		release()
		deadline := time.Now().Add(5 * time.Second)
		for auth.IsBackgroundBusy(auth.RequestPasswordResetInBackground(ctx, e.svc, user.Email)) {
			if time.Now().After(deadline) {
				t.Fatal("the background never admitted a request again")
			}
			time.Sleep(5 * time.Millisecond)
		}
		awaitMail(t, e.mail, user.Email, auth.MessageResetPassword)
	})

	t.Run("a task that never returns is cancelled at its deadline", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-deadline")
		auth.SetBackgroundTimeout(e.svc, 50*time.Millisecond)
		mailer := newCtxMailer()
		auth.SetMailer(e.svc, mailer)
		if err := auth.RequestPasswordResetInBackground(ctx, e.svc, user.Email); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-mailer.returned:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the hung send ended with %v, want its deadline", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a send that waits for its context was never cancelled")
		}
	})

	t.Run("Close drains the admitted work", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-drain")
		gate := &gateMailer{inner: e.mail, entered: make(chan struct{}), release: make(chan struct{})}
		auth.SetMailer(e.svc, gate)
		if err := auth.RequestPasswordResetInBackground(ctx, e.svc, user.Email); err != nil {
			t.Fatal(err)
		}
		<-gate.entered
		time.AfterFunc(20*time.Millisecond, func() { close(gate.release) })
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := e.svc.Close(closeCtx); err != nil {
			t.Fatalf("Close with time to drain = %v", err)
		}
		if msg, ok := e.mail.Last(user.Email); !ok || msg.Kind != auth.MessageResetPassword {
			t.Fatal("Close returned before the in-flight message was sent")
		}
		if err := auth.RequestPasswordResetInBackground(ctx, e.svc, user.Email); !auth.IsBackgroundBusy(err) {
			t.Fatalf("a request after Close = %v, want it refused", err)
		}
	})

	t.Run("Close cancels what outlives its context", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-cancel")
		mailer := newCtxMailer()
		auth.SetMailer(e.svc, mailer)
		if err := auth.RequestPasswordResetInBackground(ctx, e.svc, user.Email); err != nil {
			t.Fatal(err)
		}
		<-mailer.entered
		closeCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := e.svc.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close past its deadline = %v, want the deadline", err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("Close took %v with a send waiting on its context", took)
		}
		if err := <-mailer.returned; !errors.Is(err, context.Canceled) {
			t.Fatalf("the in-flight send ended with %v, want it cancelled by Close", err)
		}
	})

	t.Run("a made-up token is refused before hashing", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		hasher := &countingHasher{}
		auth.SetHasher(e.svc, hasher)
		if _, err := e.svc.ResetPassword(ctx, "not-a-real-token", "brand-new-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a made-up token = %v, want ErrInvalidToken", err)
		}
		if hasher.hashes.Load() != 0 {
			t.Fatal("bcrypt ran for a token that cannot work")
		}
	})

	t.Run("a login in flight when the reset commits does not survive it", func(t *testing.T) {
		// The login has checked the old password and is about to start its
		// session when the reset commits: the session must not start.
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-inflight")
		token := e.resetToken(t, user)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		auth.SetLoginAuthenticatedHook(e.svc, func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		})
		type result struct {
			pair auth.TokenPair
			err  error
		}
		done := make(chan result, 1)
		go func() {
			_, pair, err := e.svc.Login(ctx, user.Email, testPassword)
			done <- result{pair, err}
		}()
		<-entered
		if _, err := e.svc.ResetPassword(ctx, token, "brand-new-horse"); err != nil {
			close(release)
			t.Fatalf("the reset while a login is in flight: %v", err)
		}
		close(release)
		got := <-done
		if !errors.Is(got.err, auth.ErrInvalidCredentials) {
			t.Fatalf("the old-password login after the reset = %v (pair issued: %v), want invalid credentials", got.err, got.pair.RefreshToken != "")
		}
		var live int64
		e.db.Model(&auth.RefreshToken{}).Where("user_id = ? AND revoked_at IS NULL", user.ID).Count(&live)
		if live != 0 {
			t.Fatalf("%d live sessions after the reset, want none", live)
		}
		var refused auth.AuditEvent
		e.db.Where("target_user_id = ? AND action = ?", user.ID, auth.AuditLoginFailed).Order("id DESC").First(&refused)
		if !strings.Contains(refused.Details, "password_changed") {
			t.Fatalf("the refused login's event = %+v", refused)
		}
	})

	t.Run("concurrent uses: one wins", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "reset-race")
		token := e.resetToken(t, user)
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = e.svc.ResetPassword(ctx, token, fmt.Sprintf("racing-horse-%d", i))
			}()
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, auth.ErrInvalidToken):
				t.Fatalf("a concurrent use: %v", err)
			}
		}
		if won != 1 {
			t.Fatalf("%d concurrent resets succeeded, want 1", won)
		}
	})
}
