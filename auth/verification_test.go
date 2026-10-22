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

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

type meView struct {
	ID            uint   `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// newVerificationApp is an auth app in mode whose messages go to mail;
// tweaks adjust its auth config.
func newVerificationApp(t *testing.T, db *database.DB, mode config.AuthMode, mail auth.Mailer, env config.Environment, tweaks ...func(*config.AuthConfig)) *framework.App {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatalf("auth.Migrate() error = %v", err)
	}
	cfg := config.DefaultFor(env)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = mode
	cfg.Auth.EmailVerificationURL = "https://app.example.com/verify-email?token={token}"
	for _, tweak := range tweaks {
		tweak(&cfg.Auth)
	}
	opts := []framework.Option{framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop())}
	if mail != nil {
		opts = append(opts, framework.WithAuthMailer(mail))
	}
	app, err := framework.New(opts...)
	if err != nil {
		t.Fatalf("framework.New() error = %v", err)
	}
	return app
}

func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) meView {
	t.Helper()
	var env dataBody[meView]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode user: %v; body: %s", err, rec.Body.String())
	}
	return env.Data
}

func TestEmailVerificationBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runEmailVerificationBearer(t, openSQLite(t))
}

// runEmailVerificationBearer: registration mails a verification link (the
// configured URL with the token); /me says unverified until the token is
// posted; the token works once; a resend within a minute is a 429 with
// Retry-After, and after verification a 409.
func runEmailVerificationBearer(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeJWT, mail, config.EnvironmentTest)
	email := fmt.Sprintf("verify-%d@example.com", time.Now().UnixNano())
	registerUser(t, app, email, testPassword)
	msg, ok := mail.Last(email)
	if !ok || msg.Kind != auth.MessageVerifyEmail || msg.Token == "" {
		t.Fatalf("registration sent %+v (%v), want a verification message", msg, ok)
	}
	if msg.URL != "https://app.example.com/verify-email?token="+msg.Token || !strings.Contains(msg.Text, msg.URL) {
		t.Fatalf("message URL %q / text %q, want the configured link with the token", msg.URL, msg.Text)
	}
	pair := loginUser(t, app, email, testPassword)
	me := bearerRequest(app, http.MethodGet, "/api/v1/me", pair.AccessToken)
	if got := decodeMe(t, me); got.Email != email || got.EmailVerified {
		t.Fatalf("/me before verification = %+v, want unverified", got)
	}

	resend := bearerPost(app, "/api/v1/auth/verify-email/resend", pair.AccessToken, "")
	assertError(t, resend, http.StatusTooManyRequests, "rate_limited")
	if resend.Header().Get("Retry-After") == "" {
		t.Fatalf("429 without Retry-After: %v", resend.Header())
	}
	if unauth := bearerPost(app, "/api/v1/auth/verify-email/resend", "", ""); unauth.Code != http.StatusUnauthorized {
		t.Fatalf("resend without a token = %d, want 401", unauth.Code)
	}

	bad := postJSON(t, app, "/api/v1/auth/verify-email", `{"token":"not-a-token"}`)
	assertError(t, bad, http.StatusUnprocessableEntity, contract.CodeValidationError)

	ok1 := postJSON(t, app, "/api/v1/auth/verify-email", `{"token":"`+msg.Token+`"}`)
	if ok1.Code != http.StatusOK || !decodeMe(t, ok1).EmailVerified {
		t.Fatalf("verify = %d %s", ok1.Code, ok1.Body.String())
	}
	again := postJSON(t, app, "/api/v1/auth/verify-email", `{"token":"`+msg.Token+`"}`)
	assertError(t, again, http.StatusUnprocessableEntity, contract.CodeValidationError)

	if got := decodeMe(t, bearerRequest(app, http.MethodGet, "/api/v1/me", pair.AccessToken)); !got.EmailVerified {
		t.Fatalf("/me after verification = %+v, want verified", got)
	}
	done := bearerPost(app, "/api/v1/auth/verify-email/resend", pair.AccessToken, "")
	assertError(t, done, http.StatusConflict, "conflict")
}

func TestEmailVerificationCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runEmailVerificationCookie(t, openSQLite(t))
}

// runEmailVerificationCookie is the cookie-mode surface: the same routes,
// the resend gated on the session cookie (and CSRF, as every unsafe
// request).
func runEmailVerificationCookie(t *testing.T, db *database.DB) {
	t.Helper()
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeCookie, mail, config.EnvironmentTest)
	email := fmt.Sprintf("cookie-verify-%d@example.com", time.Now().UnixNano())
	jar := fetchCSRF(t, app)
	registerCookieUser(t, app, jar, email, testPassword)
	if rec := loginCookieUser(t, app, jar, email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	msg, ok := mail.Last(email)
	if !ok {
		t.Fatal("registration sent no verification message")
	}
	if rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/verify-email/resend", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("resend within a minute = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, nil, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"`+msg.Token+`"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("verify without the CSRF header = %d, want 403", rec.Code)
	}
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/verify-email", `{"token":"`+msg.Token+`"}`)
	if rec.Code != http.StatusOK || !decodeMe(t, rec).EmailVerified {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeMe(t, doRequest(app, jar, http.MethodGet, "/api/v1/me", "")); !got.EmailVerified {
		t.Fatalf("/me after verification = %+v", got)
	}
}

func bearerPost(app *framework.App, path, access, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	app.Router().ServeHTTP(rec, req)
	return rec
}

func TestEmailVerificationService(t *testing.T) {
	runEmailVerificationService(t, openSQLite(t))
}

// verificationEnv is a service over db with a recording mailer and a clock
// the test moves.
type verificationEnv struct {
	svc   *auth.Service
	mail  *authtest.Mailer
	clock *stepClock
	db    *database.DB
}

func newVerificationEnv(t *testing.T, db *database.DB) verificationEnv {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.EmailVerificationTTL = time.Hour
	mail := &authtest.Mailer{}
	svc, err := auth.NewService(db.DB, cfg, auth.WithMailer(mail))
	if err != nil {
		t.Fatal(err)
	}
	clock := &stepClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	auth.SetClock(svc, clock)
	return verificationEnv{svc: svc, mail: mail, clock: clock, db: db}
}

func (e verificationEnv) register(t *testing.T, name string) (auth.User, string) {
	t.Helper()
	email := fmt.Sprintf("%s-%d@example.com", name, time.Now().UnixNano())
	user, err := e.svc.Register(context.Background(), email, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := e.mail.Last(email)
	if !ok {
		t.Fatalf("no verification message for %s", email)
	}
	return user, msg.Token
}

func runEmailVerificationService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()

	t.Run("expires", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		_, token := e.register(t, "expiry")
		e.clock.now = e.clock.now.Add(time.Hour)
		if _, err := e.svc.VerifyEmail(ctx, token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("VerifyEmail after the TTL = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("single use", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		_, token := e.register(t, "once")
		if user, err := e.svc.VerifyEmail(ctx, token); err != nil || !user.EmailVerified() {
			t.Fatalf("VerifyEmail = %+v, %v", user, err)
		}
		if _, err := e.svc.VerifyEmail(ctx, token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("second VerifyEmail = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("concurrent uses: one wins", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		_, token := e.register(t, "race")
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.svc.VerifyEmail(ctx, token); err == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d concurrent uses succeeded, want exactly 1", wins)
		}
	})

	t.Run("resend supersedes, at most once a minute", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, first := e.register(t, "resend")
		tooSoon := e.svc.SendEmailVerification(ctx, user)
		if tooSoon == nil {
			t.Fatal("a resend within a minute was sent")
		}
		e.clock.now = e.clock.now.Add(time.Minute)
		if err := e.svc.SendEmailVerification(ctx, user); err != nil {
			t.Fatal(err)
		}
		second, _ := e.mail.Last(user.Email)
		if second.Token == first {
			t.Fatal("the resend carried the same token")
		}
		if _, err := e.svc.VerifyEmail(ctx, first); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("the superseded token = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.VerifyEmail(ctx, second.Token); err != nil {
			t.Fatalf("the new token: %v", err)
		}
		if err := e.svc.SendEmailVerification(ctx, user); !errors.Is(err, auth.ErrAlreadyVerified) {
			t.Fatalf("resend after verification = %v, want ErrAlreadyVerified", err)
		}
	})

	t.Run("bound to the address", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, token := e.register(t, "moved")
		if err := e.db.Model(&auth.User{}).Where("id = ?", user.ID).Update("email", "elsewhere-"+user.Email).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.VerifyEmail(ctx, token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a token for the old address = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("a verification lapses when the email changes", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, token := e.register(t, "lapse")
		if _, err := e.svc.VerifyEmail(ctx, token); err != nil {
			t.Fatal(err)
		}
		// An ordinary update, outside the framework: the new address is not
		// proven, so the user is not verified.
		moved := "moved-" + user.Email
		if err := e.db.Model(&auth.User{}).Where("id = ?", user.ID).Update("email", moved).Error; err != nil {
			t.Fatal(err)
		}
		var reloaded auth.User
		if err := e.db.First(&reloaded, user.ID).Error; err != nil {
			t.Fatal(err)
		}
		if reloaded.EmailVerified() {
			t.Fatal("the user is verified for an address nobody proved")
		}
		e.clock.now = e.clock.now.Add(time.Minute)
		if err := e.svc.SendEmailVerification(ctx, reloaded); err != nil {
			t.Fatalf("a resend for the new address: %v", err)
		}
		msg, ok := e.mail.Last(moved)
		if !ok {
			t.Fatal("no message to the new address")
		}
		verified, err := e.svc.VerifyEmail(ctx, msg.Token)
		if err != nil || !verified.EmailVerified() || verified.VerifiedEmail != moved {
			t.Fatalf("verifying the new address = %+v, %v", verified, err)
		}
	})

	t.Run("a message in flight holds the wait", func(t *testing.T) {
		// The second resend finds the first's pending token: it waits out the
		// interval instead of mailing again while the first is sending.
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "inflight")
		e.clock.now = e.clock.now.Add(time.Minute)
		gate := &gateMailer{inner: e.mail, entered: make(chan struct{}), release: make(chan struct{})}
		auth.SetMailer(e.svc, gate)
		done := make(chan error, 1)
		go func() { done <- e.svc.SendEmailVerification(ctx, user) }()
		<-gate.entered
		err := e.svc.SendEmailVerification(ctx, user)
		close(gate.release)
		if !auth.IsResendTooSoon(err) {
			t.Fatalf("a resend while another is sending = %v, want the resend wait", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("the resend in flight: %v", err)
		}
	})

	t.Run("overlapping sends: the wait runs from the latest activation", func(t *testing.T) {
		// A is issued, and its mailer is slow past the interval; B is issued
		// then, delivered and activated at +60s; A is delivered at +70s and
		// supersedes B. The interval runs from A's activation, not from B,
		// the later-issued one: a resend at +120s waits, one at +130s goes.
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "overlap")
		start := e.clock.now.Add(time.Minute)
		clock := &syncClock{now: start}
		auth.SetClock(e.svc, clock)
		gate := &gateMailer{inner: e.mail, entered: make(chan struct{}), release: make(chan struct{})}
		auth.SetMailer(e.svc, gate)

		doneA := make(chan error, 1)
		go func() { doneA <- e.svc.SendEmailVerification(ctx, user) }()
		<-gate.entered
		clock.set(start.Add(time.Minute))
		if err := e.svc.SendEmailVerification(ctx, user); err != nil {
			close(gate.release)
			t.Fatalf("B, issued once A's pending interval passed: %v", err)
		}
		b, _ := e.mail.Last(user.Email)
		clock.set(start.Add(70 * time.Second))
		close(gate.release)
		if err := <-doneA; err != nil {
			t.Fatalf("A, delivered after B: %v", err)
		}
		a, _ := e.mail.Last(user.Email)
		if a.Token == b.Token {
			t.Fatal("A's message was not the last one delivered")
		}

		clock.set(start.Add(2 * time.Minute))
		if err := e.svc.SendEmailVerification(ctx, user); !auth.IsResendTooSoon(err) {
			t.Fatalf("a resend 50s after A's activation = %v, want the resend wait", err)
		}
		if _, err := e.svc.VerifyEmail(ctx, b.Token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("B, superseded by the later activation = %v, want ErrInvalidToken", err)
		}
		clock.set(start.Add(130 * time.Second))
		if err := e.svc.SendEmailVerification(ctx, user); err != nil {
			t.Fatalf("a resend a minute after A's activation: %v", err)
		}
	})

	t.Run("a failed send is withdrawn", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		e.mail.Err = errors.New("smtp down")
		user, err := e.svc.Register(ctx, fmt.Sprintf("down-%d@example.com", time.Now().UnixNano()), testPassword)
		if err != nil {
			t.Fatalf("registration failed with the mailer: %v", err)
		}
		var count int64
		if err := e.db.Model(&auth.OneTimeToken{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("tokens after a failed send = %d (%v), want none", count, err)
		}
		e.mail.Err = nil
		if err := e.svc.SendEmailVerification(ctx, user); err != nil {
			t.Fatalf("the retry right after a failed send: %v", err)
		}
	})

	t.Run("a failed resend leaves the delivered link working", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, token := e.register(t, "flaky")
		e.clock.now = e.clock.now.Add(time.Minute)
		e.mail.Err = errors.New("smtp down")
		if err := e.svc.SendEmailVerification(ctx, user); err == nil {
			t.Fatal("the resend reported success with a failing mailer")
		}
		e.mail.Err = nil
		if _, err := e.svc.VerifyEmail(ctx, token); err != nil {
			t.Fatalf("the link delivered before the failed resend: %v", err)
		}
	})

	t.Run("verify and resend at once", func(t *testing.T) {
		// They take the same locks in the same order: on PostgreSQL and
		// MySQL the opposite order deadlocked (a 500).
		e := newVerificationEnv(t, db)
		for i := range 5 {
			user, token := e.register(t, fmt.Sprintf("both-%d", i))
			e.clock.now = e.clock.now.Add(time.Minute)
			var wg sync.WaitGroup
			var verifyErr, sendErr error
			wg.Add(2)
			go func() { defer wg.Done(); _, verifyErr = e.svc.VerifyEmail(ctx, token) }()
			go func() { defer wg.Done(); sendErr = e.svc.SendEmailVerification(ctx, user) }()
			wg.Wait()
			if verifyErr != nil && !errors.Is(verifyErr, auth.ErrInvalidToken) {
				t.Fatalf("verify alongside a resend: %v", verifyErr)
			}
			if sendErr != nil && !errors.Is(sendErr, auth.ErrAlreadyVerified) {
				t.Fatalf("resend alongside a verify: %v", sendErr)
			}
		}
	})

	t.Run("no mailer", func(t *testing.T) {
		cfg := config.DefaultFor(config.EnvironmentTest)
		cfg.Auth.JWTSecret = testJWTSecret
		cfg.Auth.BcryptCost = bcrypt.MinCost
		svc, err := auth.NewService(db.DB, cfg)
		if err != nil {
			t.Fatal(err)
		}
		user, err := svc.Register(ctx, fmt.Sprintf("nomail-%d@example.com", time.Now().UnixNano()), testPassword)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.SendEmailVerification(ctx, user); !errors.Is(err, auth.ErrNoMailer) {
			t.Fatalf("SendEmailVerification without a mailer = %v, want ErrNoMailer", err)
		}
	})

	t.Run("a superuser starts verified", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		email := fmt.Sprintf("root-%d@example.com", time.Now().UnixNano())
		user, err := e.svc.CreateSuperuser(ctx, email, testPassword)
		if err != nil || !user.EmailVerified() {
			t.Fatalf("CreateSuperuser = %+v, %v; want verified", user, err)
		}
		if _, ok := e.mail.Last(email); ok {
			t.Fatal("createsuperuser sent a verification message")
		}
	})
}

// gateMailer holds its first Send until release, then sends through inner;
// later Sends go straight through, while the first is still held.
type gateMailer struct {
	inner   auth.Mailer
	held    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (m *gateMailer) Send(ctx context.Context, msg auth.Message) error {
	if m.held.CompareAndSwap(false, true) {
		close(m.entered)
		<-m.release
	}
	return m.inner.Send(ctx, msg)
}

// TestRequireVerifiedEmail: an application's own operation behind the
// Bearer gate and RequireVerifiedEmail answers 403 for an unverified user,
// 200 once verified, and 401 without the gate's user.
func TestRequireVerifiedEmail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	mail := &authtest.Mailer{}
	app := newVerificationApp(t, db, config.AuthModeJWT, mail, config.EnvironmentTest)
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	svc, err := auth.NewService(db.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	type out struct {
		Body struct {
			OK bool `json:"ok"`
		}
	}
	handler := func(context.Context, *struct{}) (*out, error) {
		o := &out{}
		o.Body.OK = true
		return o, nil
	}
	huma.Register(app.API(), huma.Operation{
		OperationID: "verified-only", Method: http.MethodPost, Path: "/api/v1/verified-only",
		Middlewares: huma.Middlewares{svc.RequireBearer(), auth.RequireVerifiedEmail()},
	}, handler)
	huma.Register(app.API(), huma.Operation{
		OperationID: "ungated", Method: http.MethodPost, Path: "/api/v1/ungated",
		Middlewares: huma.Middlewares{auth.RequireVerifiedEmail()},
	}, handler)

	email := fmt.Sprintf("gate-%d@example.com", time.Now().UnixNano())
	registerUser(t, app, email, testPassword)
	pair := loginUser(t, app, email, testPassword)
	assertError(t, bearerPost(app, "/api/v1/verified-only", pair.AccessToken, ""), http.StatusForbidden, "authorization")
	msg, _ := mail.Last(email)
	if rec := postJSON(t, app, "/api/v1/auth/verify-email", `{"token":"`+msg.Token+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("verify = %d", rec.Code)
	}
	if rec := bearerPost(app, "/api/v1/verified-only", pair.AccessToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("verified user = %d %s, want 200", rec.Code, rec.Body.String())
	}
	// The email changes outside the framework: the gate denies again.
	if err := db.Model(&auth.User{}).Where("email = ?", email).Update("email", "other-"+email).Error; err != nil {
		t.Fatal(err)
	}
	assertError(t, bearerPost(app, "/api/v1/verified-only", pair.AccessToken, ""), http.StatusForbidden, "authorization")
	assertError(t, bearerPost(app, "/api/v1/ungated", pair.AccessToken, ""), http.StatusUnauthorized, "authentication")
}

// TestProductionWithoutMailer: a production app with no mailer sends no
// message at registration (it still succeeds) and answers a resend with 503.
func TestProductionWithoutMailer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	app := newVerificationApp(t, db, config.AuthModeJWT, nil, config.EnvironmentProduction)
	email := fmt.Sprintf("prod-%d@example.com", time.Now().UnixNano())
	registerUser(t, app, email, testPassword)
	pair := loginUser(t, app, email, testPassword)
	assertError(t, bearerPost(app, "/api/v1/auth/verify-email/resend", pair.AccessToken, ""), http.StatusServiceUnavailable, "dependency_unavailable")
	if _, err := framework.New(framework.WithAuthMailer(nil)); err == nil {
		t.Fatal("WithAuthMailer(nil) was accepted")
	}
}
