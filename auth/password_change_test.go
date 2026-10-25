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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

const changedPassword = "brand-new-horse"

func TestPasswordChangeService(t *testing.T) {
	runPasswordChangeService(t, openSQLite(t))
}

// sessionOf is the refresh token row the access token's session started at,
// as the gate records it for a request.
func sessionOf(t *testing.T, svc *auth.Service, access string) uint {
	t.Helper()
	row, err := auth.AuthenticatedRow(context.Background(), svc, access)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func runPasswordChangeService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()

	t.Run("a wrong current password changes nothing, and is audited", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-wrong")
		if _, err := e.svc.ChangePassword(ctx, user.ID, "not-my-password", changedPassword, auth.PasswordChange{}); !errors.Is(err, auth.ErrWrongCurrentPassword) {
			t.Fatalf("ChangePassword(wrong current) = %v, want ErrWrongCurrentPassword", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("the password after a refused change: %v", err)
		}
		var failed auth.AuditEvent
		e.db.Where("target_user_id = ? AND action = ?", user.ID, auth.AuditPasswordChangeFailed).First(&failed)
		if failed.ID == 0 || !strings.Contains(failed.Details, "wrong_current_password") {
			t.Fatalf("the refused change was not audited: %+v", failed)
		}
	})

	t.Run("signs the other sessions out, keeps the requester's", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-default")
		mine, err := e.svc.IssueTokens(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		other, err := e.svc.IssueTokens(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		revoked, err := e.svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{Session: sessionOf(t, e.svc, mine.AccessToken)})
		if err != nil || !revoked {
			t.Fatalf("ChangePassword = %v, %v; want the others revoked", revoked, err)
		}
		if _, err := e.svc.RotateRefresh(ctx, other.RefreshToken); err == nil {
			t.Fatal("another session survived the password change")
		}
		if _, err := e.svc.RotateRefresh(ctx, mine.RefreshToken); err != nil {
			t.Fatalf("the requester's session after its own change: %v", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err == nil {
			t.Fatal("the old password still works")
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, changedPassword); err != nil {
			t.Fatalf("the new password: %v", err)
		}
		var changed auth.AuditEvent
		e.db.Where("target_user_id = ? AND action = ?", user.ID, auth.AuditPasswordChanged).First(&changed)
		if changed.ID == 0 || !strings.Contains(changed.Details, `"other_sessions_revoked":"true"`) {
			t.Fatalf("the change was not audited: %+v", changed)
		}
	})

	t.Run("keeps the other sessions when asked", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-keep")
		mine, _ := e.svc.IssueTokens(ctx, user)
		other, _ := e.svc.IssueTokens(ctx, user)
		revoked, err := e.svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{KeepOtherSessions: true, Session: sessionOf(t, e.svc, mine.AccessToken)})
		if err != nil || revoked {
			t.Fatalf("ChangePassword(keep) = %v, %v", revoked, err)
		}
		if _, err := e.svc.RotateRefresh(ctx, other.RefreshToken); err != nil {
			t.Fatalf("a kept session: %v", err)
		}
	})

	t.Run("with no requesting session, every session ends", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-nosession")
		pair, _ := e.svc.IssueTokens(ctx, user)
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); err == nil {
			t.Fatal("a session survived a change asked by no session")
		}
	})

	t.Run("an outstanding reset link stops working", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-voids-reset")
		token := e.resetToken(t, user)
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ResetPassword(ctx, token, "attacker-horse"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reset link mailed before the change = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("the password policy applies", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-policy")
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, strings.Repeat("x", 73), auth.PasswordChange{}); err == nil {
			t.Fatal("a password over bcrypt's 72 bytes was accepted")
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("the password after a refused change: %v", err)
		}
	})

	t.Run("a change asked by a session that has ended changes nothing", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-ended")
		mine, _ := e.svc.IssueTokens(ctx, user)
		row := sessionOf(t, e.svc, mine.AccessToken)
		// The session ends between the request's gate and the change's lock.
		if err := e.svc.RevokeAllSessions(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{Session: row}); !errors.Is(err, auth.ErrSessionEnded) {
			t.Fatalf("ChangePassword from an ended session = %v, want ErrSessionEnded", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("the password after a change from an ended session: %v (it must be unchanged)", err)
		}
	})

	t.Run("a reset that commits during the change wins; the change is refused", func(t *testing.T) {
		// The change checked the current password, then a reset set another
		// before its transaction: the checked password is no longer the
		// user's, so the change must not overwrite the reset.
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "change-race")
		token := e.resetToken(t, user)
		auth.SetPasswordChangeVerifiedHook(e.svc, func() {
			if _, err := e.svc.ResetPassword(ctx, token, "reset-horse"); err != nil {
				t.Errorf("the reset during the change: %v", err)
			}
		})
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{}); !errors.Is(err, auth.ErrPasswordChanged) {
			t.Fatalf("ChangePassword across a reset = %v, want ErrPasswordChanged", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, "reset-horse"); err != nil {
			t.Fatalf("the reset's password: %v", err)
		}
	})
}

// TestPasswordChangeNeverLogsPasswords: whatever happens, no log line and no
// audit row holds the current or the new password.
func TestPasswordChangeNeverLogsPasswords(t *testing.T) {
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	svc, err := auth.NewService(db.DB, cfg, auth.WithMailer(&authtest.Mailer{}), auth.WithLogger(zap.New(core)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, err := svc.Register(ctx, fmt.Sprintf("change-logs-%d@example.com", time.Now().UnixNano()), testPassword)
	if err != nil {
		t.Fatal(err)
	}
	const wrong = "wrong-current-pw"
	_, _ = svc.ChangePassword(ctx, user.ID, wrong, changedPassword, auth.PasswordChange{})
	_, _ = svc.ChangePassword(ctx, user.ID, testPassword, strings.Repeat("y", 80), auth.PasswordChange{})
	if _, err := svc.ChangePassword(ctx, user.ID, testPassword, changedPassword, auth.PasswordChange{}); err != nil {
		t.Fatal(err)
	}
	secrets := []string{testPassword, wrong, changedPassword, strings.Repeat("y", 80)}
	for _, entry := range logs.All() {
		line := fmt.Sprintf("%s %v", entry.Message, entry.ContextMap())
		for _, secret := range secrets {
			if strings.Contains(line, secret) {
				t.Fatalf("a log line holds a password: %s", line)
			}
		}
	}
	var events []auth.AuditEvent
	db.Where("target_user_id = ?", user.ID).Find(&events)
	for _, e := range events {
		row := fmt.Sprintf("%+v", e)
		for _, secret := range secrets {
			if strings.Contains(row, secret) {
				t.Fatalf("an audit row holds a password: %s", row)
			}
		}
	}
}

func TestPasswordChangeBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runPasswordChangeBearer(t, openSQLite(t))
}

// changeRequest is POST /auth/password/change with a Bearer access token.
func changeRequest(app interface {
	Router() *gin.Engine
}, access, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	app.Router().ServeHTTP(rec, req)
	return rec
}

// runPasswordChangeBearer: the route needs a session and the current
// password; it signs the other sessions out and keeps this one.
func runPasswordChangeBearer(t *testing.T, db *database.DB) {
	t.Helper()
	app := newVerificationApp(t, db, config.AuthModeJWT, &authtest.Mailer{}, config.EnvironmentTest)
	email := fmt.Sprintf("change-http-%d@example.com", time.Now().UnixNano())
	registerUser(t, app, email, testPassword)
	mine := loginUser(t, app, email, testPassword)
	other := loginUser(t, app, email, testPassword)

	assertError(t, changeRequest(app, "", `{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`), http.StatusUnauthorized, "authentication")
	wrong := changeRequest(app, mine.AccessToken, `{"current_password":"not-my-password","new_password":"`+changedPassword+`"}`)
	assertError(t, wrong, http.StatusUnprocessableEntity, "validation_error")
	if !strings.Contains(wrong.Body.String(), "current_password") {
		t.Fatalf("a wrong current password names no field: %s", wrong.Body.String())
	}
	assertError(t, changeRequest(app, mine.AccessToken, `{"current_password":"`+testPassword+`","new_password":"short"}`), http.StatusUnprocessableEntity, "validation_error")

	rec := changeRequest(app, mine.AccessToken, `{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("change = %d %s", rec.Code, rec.Body.String())
	}
	var result dataBody[struct {
		OK                   bool `json:"ok"`
		OtherSessionsRevoked bool `json:"other_sessions_revoked"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.Data.OK || !result.Data.OtherSessionsRevoked {
		t.Fatalf("change body = %s", rec.Body.String())
	}
	me := func(access string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		app.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := me(mine.AccessToken); code != http.StatusOK {
		t.Fatalf("/me on the session that changed the password = %d, want 200", code)
	}
	if code := me(other.AccessToken); code != http.StatusUnauthorized {
		t.Fatalf("/me on another session after the change = %d, want 401", code)
	}
	loginUser(t, app, email, changedPassword)
}

func TestPasswordChangeCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runPasswordChangeCookie(t, openSQLite(t))
}

// runPasswordChangeCookie: the same route in cookie mode, behind CSRF; the
// browser's own session stays signed in.
func runPasswordChangeCookie(t *testing.T, db *database.DB) {
	t.Helper()
	app := newVerificationApp(t, db, config.AuthModeCookie, &authtest.Mailer{}, config.EnvironmentTest)
	email := fmt.Sprintf("change-cookie-%d@example.com", time.Now().UnixNano())
	jar := fetchCSRF(t, app)
	registerCookieUser(t, app, jar, email, testPassword)
	if rec := loginCookieUser(t, app, jar, email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	wrong := doRequest(app, jar, http.MethodPost, "/api/v1/auth/password/change", `{"current_password":"not-my-password","new_password":"`+changedPassword+`"}`)
	if wrong.Code != http.StatusUnprocessableEntity || !strings.Contains(wrong.Body.String(), "current_password") {
		t.Fatalf("a wrong current password in cookie mode = %d %s, want a 422 on current_password", wrong.Code, wrong.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodGet, "/api/v1/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("/me after a refused change = %d: a wrong password must not sign the browser out", rec.Code)
	}
	body := `{"current_password":"` + testPassword + `","new_password":"` + changedPassword + `"}`
	if rec := doRequest(app, nil, http.MethodPost, "/api/v1/auth/password/change", body); rec.Code != http.StatusForbidden {
		t.Fatalf("change without the CSRF header = %d, want 403", rec.Code)
	}
	if rec := doRequest(app, jar, http.MethodPost, "/api/v1/auth/password/change", body); rec.Code != http.StatusOK {
		t.Fatalf("change = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, jar, http.MethodGet, "/api/v1/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("/me after the change = %d, want this session kept", rec.Code)
	}
}

// TestPasswordChangeRaceIs409: over HTTP, a change whose checked password was
// replaced meanwhile is a 409 (try again), not a 401 that would sign the
// browser out, nor a 500.
func TestPasswordChangeRaceIs409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	router := gin.New()
	api := humagin.New(router, huma.DefaultConfig("race", "1.0.0"))
	svc, err := auth.Mount(api, db.DB, cfg, auth.WithMailer(&authtest.Mailer{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, err := svc.Register(ctx, fmt.Sprintf("change-409-%d@example.com", time.Now().UnixNano()), testPassword)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := svc.IssueTokens(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	auth.SetPasswordChangeVerifiedHook(svc, func() {
		// Another change commits between this one's check and its lock.
		if err := db.Model(&auth.User{}).Where("id = ?", user.ID).Update("password_hash", "replaced").Error; err != nil {
			t.Error(err)
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change", strings.NewReader(`{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a change across another = %d %s, want 409", rec.Code, rec.Body.String())
	}
}
