package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

func TestMFAService(t *testing.T) {
	runMFAService(t, openSQLite(t))
}

// enrollMFA enrolls user's TOTP with the env's clock and returns the secret
// and the recovery codes.
func (e verificationEnv) enrollMFA(t *testing.T, user auth.User) (string, []string) {
	t.Helper()
	ctx := context.Background()
	enrollment, err := e.svc.BeginTOTPEnrollment(ctx, user.ID, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := e.svc.ConfirmTOTPEnrollment(ctx, user.ID, auth.TOTPCode(enrollment.Secret, e.clock.now))
	if err != nil {
		t.Fatal(err)
	}
	return enrollment.Secret, codes
}

func runMFAService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()

	t.Run("enrolled, a login needs a code from the authenticator", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-login")
		if _, err := e.svc.BeginTOTPEnrollment(ctx, user.ID, "not-it"); !errors.Is(err, auth.ErrWrongCurrentPassword) {
			t.Fatalf("enrollment with a wrong password = %v", err)
		}
		enrollment, err := e.svc.BeginTOTPEnrollment(ctx, user.ID, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(enrollment.URI)
		if err != nil || uri.Scheme != "otpauth" || uri.Host != "totp" || uri.Query().Get("secret") != enrollment.Secret || !strings.Contains(uri.Path, user.Email) {
			t.Fatalf("otpauth URI = %q (%v)", enrollment.URI, err)
		}
		// Pending: the login does not change until it is confirmed.
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("login with a pending enrollment = %v", err)
		}
		if _, err := e.svc.ConfirmTOTPEnrollment(ctx, user.ID, "000000"); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("confirming with a wrong code = %v", err)
		}
		codes, err := e.svc.ConfirmTOTPEnrollment(ctx, user.ID, auth.TOTPCode(enrollment.Secret, e.clock.now))
		if err != nil || len(codes) != 10 {
			t.Fatalf("confirm = %d codes, %v", len(codes), err)
		}
		st, err := e.svc.MFAStatusOf(ctx, user.ID)
		if err != nil || !st.Enabled || st.PendingEnrollment || st.RecoveryCodesLeft != 10 {
			t.Fatalf("status = %+v, %v", st, err)
		}
		// No code: required, after the right password only.
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); !errors.Is(err, auth.ErrMFARequired) {
			t.Fatalf("login without a code = %v, want ErrMFARequired", err)
		}
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, "wrong-horse", "123456"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("a wrong password with a code = %v, want invalid credentials", err)
		}
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, "000000"); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("a wrong code = %v", err)
		}
		// The confirming code's step was used: a code of the next step works,
		// once.
		e.clock.now = e.clock.now.Add(30 * time.Second)
		code := auth.TOTPCode(enrollment.Secret, e.clock.now)
		if _, _, err := e.svc.LoginMFA(ctx, user.Email, testPassword, code); err != nil {
			t.Fatalf("login with the code = %v", err)
		}
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, code); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("the same code again = %v, want ErrInvalidMFACode (a replay)", err)
		}
		// Within the skew: the previous step's code is refused once a later
		// one was used.
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, auth.TOTPCode(enrollment.Secret, e.clock.now.Add(-30*time.Second))); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("an older step's code = %v, want refused", err)
		}
		if _, err := e.svc.BeginTOTPEnrollment(ctx, user.ID, testPassword); !errors.Is(err, auth.ErrMFAAlreadyEnabled) {
			t.Fatalf("enrolling again = %v, want ErrMFAAlreadyEnabled", err)
		}
	})

	t.Run("recovery codes are single-use and can be replaced", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-recovery")
		_, codes := e.enrollMFA(t, user)
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, strings.ToUpper(codes[0])); err != nil {
			t.Fatalf("login with a recovery code (typed in capitals) = %v", err)
		}
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, codes[0]); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("the same recovery code again = %v, want ErrInvalidMFACode", err)
		}
		if st, _ := e.svc.MFAStatusOf(ctx, user.ID); st.RecoveryCodesLeft != 9 {
			t.Fatalf("recovery codes left = %d, want 9", st.RecoveryCodesLeft)
		}
		// New codes are a second factor: the password alone does not mint them.
		if _, err := e.svc.RegenerateRecoveryCodes(ctx, user.ID, testPassword, ""); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("regenerate without a code = %v, want ErrInvalidMFACode", err)
		}
		if _, err := e.svc.RegenerateRecoveryCodes(ctx, user.ID, testPassword, "000000"); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("regenerate with a wrong code = %v, want ErrInvalidMFACode", err)
		}
		fresh, err := e.svc.RegenerateRecoveryCodes(ctx, user.ID, testPassword, codes[2])
		if err != nil || len(fresh) != 10 {
			t.Fatalf("regenerate = %d, %v", len(fresh), err)
		}
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, codes[1]); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("an old recovery code after regenerating = %v, want refused", err)
		}
		if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, fresh[0]); err != nil {
			t.Fatalf("a new recovery code = %v", err)
		}
		var events int64
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ?", user.ID, auth.AuditMFARecoveryCodeUsed).Count(&events)
		if events != 3 {
			t.Fatalf("recovery code used events = %d, want 3 (two logins, one regeneration)", events)
		}
	})

	t.Run("disabling needs the password and a code; a reset needs neither", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-disable")
		secret, codes := e.enrollMFA(t, user)
		e.clock.now = e.clock.now.Add(30 * time.Second)
		if err := e.svc.DisableMFA(ctx, user.ID, testPassword, "000000"); !errors.Is(err, auth.ErrInvalidMFACode) {
			t.Fatalf("disable with a wrong code = %v", err)
		}
		if err := e.svc.DisableMFA(ctx, user.ID, "not-it", auth.TOTPCode(secret, e.clock.now)); !errors.Is(err, auth.ErrWrongCurrentPassword) {
			t.Fatalf("disable with a wrong password = %v", err)
		}
		if err := e.svc.DisableMFA(ctx, user.ID, testPassword, codes[0]); err != nil {
			t.Fatalf("disable with a recovery code = %v", err)
		}
		var used int64
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ?", user.ID, auth.AuditMFARecoveryCodeUsed).Count(&used)
		if used != 1 {
			t.Fatalf("recovery code used events after a disable with one = %d, want 1", used)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("login after disabling = %v", err)
		}
		if err := e.svc.DisableMFA(ctx, user.ID, testPassword, "123456"); !errors.Is(err, auth.ErrMFANotEnabled) {
			t.Fatalf("disable again = %v, want ErrMFANotEnabled", err)
		}
		secret, _ = e.enrollMFA(t, user)
		e.clock.now = e.clock.now.Add(30 * time.Second)
		_, pair, err := e.svc.LoginMFA(ctx, user.Email, testPassword, auth.TOTPCode(secret, e.clock.now))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.ResetMFA(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		// The lost device's session ends with the reset.
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("a session from before the reset = %v, want revoked", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("login after a reset = %v", err)
		}
		var resets int64
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ?", user.ID, auth.AuditMFAReset).Count(&resets)
		if resets != 1 {
			t.Fatalf("reset events = %d, want 1", resets)
		}
	})

	t.Run("wrong codes are throttled per account", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-throttle")
		secret, _ := e.enrollMFA(t, user)
		for i := range 5 {
			// From a new address each: only the per-account rule holds.
			if _, err := e.svc.AuthenticateMFA(auth.ContextWithAddress(ctx, fmt.Sprintf("198.51.100.%d", 150+i)), user.Email, testPassword, "000000"); !errors.Is(err, auth.ErrInvalidMFACode) {
				t.Fatalf("wrong code %d = %v", i+1, err)
			}
		}
		e.clock.now = e.clock.now.Add(30 * time.Second)
		_, err := e.svc.AuthenticateMFA(auth.ContextWithAddress(ctx, "198.51.100.199"), user.Email, testPassword, auth.TOTPCode(secret, e.clock.now))
		throttled(t, err, "MFAPerUser")
	})

	t.Run("one code, two logins at once: one gets in", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-race")
		secret, _ := e.enrollMFA(t, user)
		e.clock.now = e.clock.now.Add(30 * time.Second)
		code := auth.TOTPCode(secret, e.clock.now)
		var wg sync.WaitGroup
		var ok, replayed atomic.Int64
		// Four: within MFAPerUser's five places, all in flight at once.
		for i := range 4 {
			wg.Go(func() {
				_, _, err := e.svc.LoginMFA(auth.ContextWithAddress(ctx, fmt.Sprintf("198.51.100.%d", 170+i)), user.Email, testPassword, code)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, auth.ErrInvalidMFACode):
					replayed.Add(1)
				default:
					t.Errorf("login = %v", err)
				}
			})
		}
		wg.Wait()
		if ok.Load() != 1 || replayed.Load() != 3 {
			t.Fatalf("logins with one code: %d in, %d refused; want 1 and 3", ok.Load(), replayed.Load())
		}
	})

	t.Run("enabling MFA ends the sessions started on the password alone", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-sessions")
		_, here, err := e.svc.Login(ctx, user.Email, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		_, stolen, err := e.svc.Login(ctx, user.Email, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		hereRow, err := auth.AuthenticatedRow(ctx, e.svc, here.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		enrollment, err := e.svc.BeginTOTPEnrollment(ctx, user.ID, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		// Confirmed from the "here" session: it stays, the other ends.
		asHere := auth.ContextWithSession(ctx, user, hereRow)
		if _, err := e.svc.ConfirmTOTPEnrollment(asHere, user.ID, auth.TOTPCode(enrollment.Secret, e.clock.now)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.RotateRefresh(ctx, stolen.RefreshToken); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("a session from before the enrollment = %v, want revoked", err)
		}
		if _, err := auth.AuthenticatedRow(ctx, e.svc, here.AccessToken); err != nil {
			t.Fatalf("the confirming session = %v, want it kept", err)
		}
		// From Go, with no session: every one ends.
		other, _ := e.register(t, "mfa-sessions-go")
		_, pair, err := e.svc.Login(ctx, other.Email, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		e.enrollMFA(t, other)
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("a session from before an enrollment confirmed from Go = %v, want revoked", err)
		}
	})

	t.Run("a login in flight across the enrollment gets no session", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-inflight")
		enrollment, err := e.svc.BeginTOTPEnrollment(ctx, user.ID, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		// The password checked, no factor found; the user confirms before
		// the session starts.
		auth.SetLoginAuthenticatedHook(e.svc, func() {
			auth.SetLoginAuthenticatedHook(e.svc, nil)
			if _, err := e.svc.ConfirmTOTPEnrollment(ctx, user.ID, auth.TOTPCode(enrollment.Secret, e.clock.now)); err != nil {
				t.Error(err)
			}
		})
		if _, _, err := e.svc.Login(ctx, user.Email, testPassword); !errors.Is(err, auth.ErrMFARequired) {
			t.Fatalf("a password-only login across the enrollment = %v, want ErrMFARequired", err)
		}
		var sessions int64
		e.db.Model(&auth.RefreshToken{}).Where("user_id = ? AND revoked_at IS NULL", user.ID).Count(&sessions)
		if sessions != 0 {
			t.Fatalf("live sessions = %d, want none", sessions)
		}
	})

	t.Run("MFAPerUser holds with the throttles turned off", func(t *testing.T) {
		e := newThrottleEnv(t, db, func(c *config.AuthConfig) { c.ThrottleDisabled = true })
		user, _ := e.register(t, "mfa-unthrottled")
		secret, _ := e.enrollMFA(t, user)
		for range 5 {
			if _, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, "000000"); !errors.Is(err, auth.ErrInvalidMFACode) {
				t.Fatal(err)
			}
		}
		e.clock.now = e.clock.now.Add(30 * time.Second)
		_, err := e.svc.AuthenticateMFA(ctx, user.Email, testPassword, auth.TOTPCode(secret, e.clock.now))
		throttled(t, err, "MFAPerUser")
	})

	t.Run("a changed MFA key: the login fails, and says why", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-rekeyed")
		secret, codes := e.enrollMFA(t, user)
		core, logs := observer.New(zap.WarnLevel)
		cfg := config.DefaultFor(config.EnvironmentTest)
		cfg.Auth.JWTSecret = testJWTSecret
		cfg.Auth.BcryptCost = bcrypt.MinCost
		cfg.Auth.MFAKey = strings.Repeat("k", 32) // was unset: derived from the JWT secret
		rekeyed, err := auth.NewService(e.db.DB, cfg, auth.WithLogger(zap.New(core)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = rekeyed.AuthenticateMFA(ctx, user.Email, testPassword, auth.TOTPCode(secret, time.Now()))
		if err == nil || errors.Is(err, auth.ErrInvalidMFACode) || errors.Is(err, auth.ErrMFARequired) {
			t.Fatalf("login after the key changed = %v, want a server error", err)
		}
		if logs.FilterMessageSnippet("cannot be decrypted").Len() != 1 {
			t.Fatalf("warnings = %v, want one naming the key change", logs.All())
		}
		// A recovery code (what the user who lost the device has) is the
		// same server error, not a wrong code charged to them: past
		// MFAPerUser's limit, still the server error, never a 429.
		for i := range auth.DefaultThrottlePolicy().MFAPerUser.Limit + 2 {
			_, err = rekeyed.AuthenticateMFA(auth.ContextWithAddress(ctx, fmt.Sprintf("198.51.100.%d", 200+i)), user.Email, testPassword, codes[0])
			if err == nil || errors.Is(err, auth.ErrInvalidMFACode) || errors.Is(err, auth.ErrTooManyAttempts) {
				t.Fatalf("recovery code attempt %d after the key changed = %v, want a server error", i+1, err)
			}
		}
		// The operator's way out: reset, and the account signs in again.
		if err := rekeyed.ResetMFA(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := rekeyed.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("login after the reset = %v", err)
		}
	})

	t.Run("a confirmation with nothing pending", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "mfa-nothing")
		if _, err := e.svc.ConfirmTOTPEnrollment(ctx, user.ID, "123456"); !errors.Is(err, auth.ErrNoPendingEnrollment) {
			t.Fatalf("confirm with nothing pending = %v", err)
		}
		if _, err := e.svc.RegenerateRecoveryCodes(ctx, user.ID, testPassword, "123456"); !errors.Is(err, auth.ErrMFANotEnabled) {
			t.Fatalf("regenerate without MFA = %v", err)
		}
	})
}

func TestMFABearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runMFABearer(t, openSQLite(t))
}

// runMFABearer: enrollment, the login challenge (a 422 on mfa_code), and
// the reset, over HTTP.
func runMFABearer(t *testing.T, db *database.DB) {
	t.Helper()
	app := newVerificationApp(t, db, config.AuthModeJWT, nil, config.EnvironmentTest)
	super, manager, plain := accountActors(t, app, db)
	pair := loginUser(t, app, plain.Email, testPassword)
	rec := bearerPost(app, "/api/v1/auth/mfa/totp", pair.AccessToken, `{"current_password":"`+testPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body.String())
	}
	var enrollment dataBody[struct {
		Secret string `json:"secret"`
		URI    string `json:"otpauth_uri"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &enrollment); err != nil || enrollment.Data.Secret == "" || !strings.HasPrefix(enrollment.Data.URI, "otpauth://totp/") {
		t.Fatalf("enroll body = %s", rec.Body.String())
	}
	rec = bearerPost(app, "/api/v1/auth/mfa/totp/confirm", pair.AccessToken, `{"mfa_code":"`+auth.TOTPCode(enrollment.Data.Secret, time.Now())+`"}`)
	var codes dataBody[struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}]
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &codes) != nil || len(codes.Data.RecoveryCodes) != 10 {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body.String())
	}
	if rec := bearerRequest(app, http.MethodGet, "/api/v1/auth/mfa", pair.AccessToken); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	login := func(code string) int {
		body := `{"email":"` + plain.Email + `","password":"` + testPassword + `"`
		if code != "" {
			body += `,"mfa_code":"` + code + `"`
		}
		return postJSON(t, app, "/api/v1/auth/login", body+"}").Code
	}
	rec = postJSON(t, app, "/api/v1/auth/login", `{"email":"`+plain.Email+`","password":"`+testPassword+`"}`)
	assertError(t, rec, http.StatusUnprocessableEntity, "validation_error")
	if !strings.Contains(rec.Body.String(), "mfa_code") {
		t.Fatalf("the challenge does not name mfa_code: %s", rec.Body.String())
	}
	if code := login(codes.Data.RecoveryCodes[0]); code != http.StatusOK {
		t.Fatalf("login with a recovery code = %d", code)
	}
	// The reset: permission, superuser rule, not one's own.
	managerPair := loginUser(t, app, manager.Email, testPassword)
	superPair := loginUser(t, app, super.Email, testPassword)
	assertError(t, bearerPost(app, fmt.Sprintf("/api/v1/auth/users/%d/mfa/reset", plain.ID), pair.AccessToken, ""), http.StatusForbidden, "authorization")
	assertError(t, bearerPost(app, fmt.Sprintf("/api/v1/auth/users/%d/mfa/reset", super.ID), managerPair.AccessToken, ""), http.StatusForbidden, "authorization")
	assertError(t, bearerPost(app, fmt.Sprintf("/api/v1/auth/users/%d/mfa/reset", super.ID), superPair.AccessToken, ""), http.StatusConflict, "conflict")
	if rec := bearerPost(app, fmt.Sprintf("/api/v1/auth/users/%d/mfa/reset", plain.ID), managerPair.AccessToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("reset by the manager = %d %s", rec.Code, rec.Body.String())
	}
	if code := login(""); code != http.StatusOK {
		t.Fatalf("login after the reset = %d", code)
	}
}

func TestMFACookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runMFACookie(t, openSQLite(t))
}

// runMFACookie: the cookie login takes mfa_code too.
func runMFACookie(t *testing.T, db *database.DB) {
	t.Helper()
	app := newVerificationApp(t, db, config.AuthModeCookie, nil, config.EnvironmentTest)
	_, _, plain := accountActors(t, app, db)
	svc, err := auth.NewService(db.DB, app.Config())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := svc.BeginTOTPEnrollment(context.Background(), plain.ID, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := svc.ConfirmTOTPEnrollment(context.Background(), plain.ID, auth.TOTPCode(enrollment.Secret, now.Add(-30*time.Second))); err != nil {
		t.Fatal(err)
	}
	jar := fetchCSRF(t, app)
	rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/login", `{"email":"`+plain.Email+`","password":"`+testPassword+`"}`)
	assertError(t, rec, http.StatusUnprocessableEntity, "validation_error")
	rec = doRequest(app, jar, http.MethodPost, "/api/v1/auth/login", `{"email":"`+plain.Email+`","password":"`+testPassword+`","mfa_code":"`+auth.TOTPCode(enrollment.Secret, now)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie login with the code = %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeMe(t, doRequest(app, jar, http.MethodGet, "/api/v1/me", "")); got.Email != plain.Email {
		t.Fatalf("/me = %+v", got)
	}
}
