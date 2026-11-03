package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

func TestAccountStateService(t *testing.T) {
	runAccountStateService(t, openSQLite(t))
}

func runAccountStateService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()

	t.Run("a disabled account can use nothing it had", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, verify := e.register(t, "disable-everything")
		_, pair, err := e.svc.Login(ctx, user.Email, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		reset := e.resetToken(t, user)
		disabled, err := e.svc.DisableUser(ctx, user.ID)
		if err != nil || !disabled.Disabled() {
			t.Fatalf("DisableUser() = %+v, %v", disabled, err)
		}
		// Told only to whoever knows the password.
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); !errors.Is(err, auth.ErrAccountDisabled) {
			t.Fatalf("the right password = %v, want ErrAccountDisabled", err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, "wrong-horse"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("a wrong password = %v, want invalid credentials", err)
		}
		// The session it had: the access token and the refresh token.
		if _, err := auth.AuthenticatedRow(ctx, e.svc, pair.AccessToken); err == nil {
			t.Fatal("the access token still authenticates")
		}
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("the refresh token = %v, want ErrInvalidRefreshToken", err)
		}
		// The links it had, and new messages.
		if _, err := e.svc.ResetPassword(ctx, reset, "new-horse-1"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reset link = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.VerifyEmail(ctx, verify); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a verification link = %v, want ErrInvalidToken", err)
		}
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		e.mail.Reset()
		if err := e.svc.RequestPasswordReset(ctx, user.Email); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.SendEmailVerification(ctx, e.reload(t, user.ID)); !errors.Is(err, auth.ErrAccountDisabled) {
			t.Fatalf("a verification message = %v, want ErrAccountDisabled", err)
		}
		if msgs := e.mail.Messages(); len(msgs) != 0 {
			t.Fatalf("a disabled account was mailed %+v", msgs)
		}
		if _, err := e.svc.ChangePassword(ctx, user.ID, testPassword, "new-horse-2", auth.PasswordChange{}); !errors.Is(err, auth.ErrAccountDisabled) {
			t.Fatalf("a password change = %v, want ErrAccountDisabled", err)
		}
		// Disabling again changes nothing, and is audited once; the revoked
		// refresh token was not taken for reuse.
		if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		var disabledEvents, reuse int64
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ?", user.ID, auth.AuditUserDisabled).Count(&disabledEvents)
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ?", user.ID, auth.AuditRefreshReuse).Count(&reuse)
		if disabledEvents != 1 || reuse != 0 {
			t.Fatalf("audit: disabled = %d, refresh reuse = %d; want 1 and 0", disabledEvents, reuse)
		}
	})

	t.Run("enabled again, the user signs in; what was revoked stays revoked", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "disable-enable")
		_, pair, err := e.svc.Login(ctx, user.Email, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		enabled, err := e.svc.EnableUser(ctx, user.ID)
		if err != nil || enabled.Disabled() {
			t.Fatalf("EnableUser() = %+v, %v", enabled, err)
		}
		if _, err := e.svc.Authenticate(ctx, user.Email, testPassword); err != nil {
			t.Fatalf("after EnableUser = %v", err)
		}
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("the refresh token from before = %v, want still revoked", err)
		}
		var enabledEvents int64
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ?", user.ID, auth.AuditUserEnabled).Count(&enabledEvents)
		if enabledEvents != 1 {
			t.Fatalf("enabled events = %d, want 1", enabledEvents)
		}
	})

	t.Run("a login racing a disable starts no session", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "disable-race")
		// The password was checked; the account is disabled before the
		// session starts.
		auth.SetLoginAuthenticatedHook(e.svc, func() {
			if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
				t.Error(err)
			}
		})
		if _, _, err := e.svc.Login(ctx, user.Email, testPassword); !errors.Is(err, auth.ErrAccountDisabled) {
			t.Fatalf("Login() racing a disable = %v, want ErrAccountDisabled", err)
		}
		var sessions, failed int64
		e.db.Model(&auth.RefreshToken{}).Where("user_id = ?", user.ID).Count(&sessions)
		e.db.Model(&auth.AuditEvent{}).Where("target_user_id = ? AND action = ? AND details LIKE ?", user.ID, auth.AuditLoginFailed, "%account_disabled%").Count(&failed)
		if sessions != 0 || failed != 1 {
			t.Fatalf("sessions = %d, account_disabled login failures = %d; want 0 and 1", sessions, failed)
		}
	})

	t.Run("a session that outlived the disable is refused too", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "disable-restored")
		_, pair, err := e.svc.Login(ctx, user.Email, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		// Revived behind the Service's back (a restore, a stray write).
		if err := e.db.Model(&auth.RefreshToken{}).Where("user_id = ?", user.ID).Update("revoked_at", nil).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := auth.AuthenticatedRow(ctx, e.svc, pair.AccessToken); err == nil {
			t.Fatal("a disabled account's revived session authenticates")
		}
		if _, err := e.svc.RotateRefresh(ctx, pair.RefreshToken); !errors.Is(err, auth.ErrInvalidRefreshToken) {
			t.Fatalf("a disabled account's revived refresh token = %v", err)
		}
	})

	t.Run("links mailed before a disable stay dead after an enable", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "disable-links")
		reset := e.resetToken(t, user)
		change := e.changeToken(t, user, fmt.Sprintf("taken-over-%d@example.com", time.Now().UnixNano()))
		if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.EnableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		// Active again, the account still refuses what was voided: an email
		// change link to an address a thief chose must not move the account.
		if _, err := e.svc.ConfirmEmailChange(ctx, change); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("an email change link from before the disable = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.ResetPassword(ctx, reset, "new-horse-3"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a reset link from before the disable = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("a link being sent when the account is disabled never works", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "disable-pending")
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		// The token is pending while its message is sent; the disable
		// lands then.
		auth.SetMailer(e.svc, sendHook{inner: e.mail, before: func(msg auth.Message) {
			if msg.Kind == auth.MessageResetPassword {
				if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
					t.Error(err)
				}
			}
		}})
		if err := e.svc.RequestPasswordReset(ctx, user.Email); err == nil {
			t.Fatal("a reset whose token was voided while pending reported success")
		}
		msg, ok := e.mail.Last(user.Email)
		if !ok || msg.Kind != auth.MessageResetPassword {
			t.Fatalf("no reset message went out (last %+v)", msg)
		}
		if _, err := e.svc.EnableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ResetPassword(ctx, msg.Token, "new-horse-4"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("the link sent during the disable = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("a link revived behind the Service's back is refused while disabled", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, verify := e.register(t, "disable-revived-link")
		reset := e.resetToken(t, user)
		if _, err := e.svc.DisableUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.db.Model(&auth.OneTimeToken{}).Where("user_id = ?", user.ID).Update("used_at", nil).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ResetPassword(ctx, reset, "new-horse-5"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a revived reset link = %v, want ErrInvalidToken", err)
		}
		if _, err := e.svc.VerifyEmail(ctx, verify); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("a revived verification link = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("a disabled account has no permission", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		user, _ := e.register(t, "disable-perms")
		permission, err := auth.EnsurePermission(ctx, e.db.DB, "test.disabled.perm", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.GrantPermissionToUser(ctx, e.db.DB, &user, &permission); err != nil {
			t.Fatal(err)
		}
		if ok, err := auth.HasPermission(ctx, e.db.DB, user, permission.Key); err != nil || !ok {
			t.Fatalf("granted, active = %v, %v", ok, err)
		}
		disabled, err := e.svc.DisableUser(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The snapshot says disabled; so does the row behind an old one.
		if ok, _ := auth.HasPermission(ctx, e.db.DB, disabled, permission.Key); ok {
			t.Fatal("a disabled account has its permission")
		}
		if ok, _ := auth.HasPermission(ctx, e.db.DB, user, permission.Key); ok {
			t.Fatal("an active snapshot of a disabled account has its permission")
		}
		super := disabled
		super.IsSuperuser = true
		if ok, _ := auth.HasPermission(ctx, e.db.DB, super, "anything"); ok {
			t.Fatal("a disabled superuser has every permission")
		}
		// A superuser loaded while active (a job holding it), disabled
		// since: the row decides, not the snapshot.
		admin, err := e.svc.CreateSuperuser(ctx, fmt.Sprintf("disable-perms-super-%d@example.com", time.Now().UnixNano()), testPassword)
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := auth.HasPermission(ctx, e.db.DB, admin, "anything"); !ok {
			t.Fatal("an active superuser lacks a permission")
		}
		if _, err := e.svc.DisableUser(ctx, admin.ID); err != nil {
			t.Fatal(err)
		}
		if ok, _ := auth.HasPermission(ctx, e.db.DB, admin, "anything"); ok {
			t.Fatal("a superuser snapshot from before the disable has every permission")
		}
		// And a demoted one's stale flag grants nothing either.
		if _, err := e.svc.EnableUser(ctx, admin.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.db.Model(&auth.User{}).Where("id = ?", admin.ID).Update("is_superuser", false).Error; err != nil {
			t.Fatal(err)
		}
		if ok, _ := auth.HasPermission(ctx, e.db.DB, admin, "anything"); ok {
			t.Fatal("a demoted superuser's snapshot has every permission")
		}
	})

	t.Run("an unknown user", func(t *testing.T) {
		e := newVerificationEnv(t, db)
		if _, err := e.svc.DisableUser(ctx, 1<<30); !errors.Is(err, auth.ErrUserNotFound) {
			t.Fatalf("DisableUser(unknown) = %v", err)
		}
		if _, err := e.svc.EnableUser(ctx, 1<<30); !errors.Is(err, auth.ErrUserNotFound) {
			t.Fatalf("EnableUser(unknown) = %v", err)
		}
	})
}

func TestAccountStateBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runAccountStateBearer(t, openSQLite(t))
}

// accountActors makes, on app's database, a superuser, a user granted
// PermissionManageUsers, and a plain user; it returns their emails and ids.
func accountActors(t *testing.T, app *framework.App, db *database.DB) (super, manager, plain auth.User) {
	t.Helper()
	ctx := context.Background()
	svc, err := auth.NewService(db.DB, app.Config())
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano()
	if super, err = svc.CreateSuperuser(ctx, fmt.Sprintf("state-super-%d@example.com", stamp), testPassword); err != nil {
		t.Fatal(err)
	}
	if manager, err = svc.Register(ctx, fmt.Sprintf("state-manager-%d@example.com", stamp), testPassword); err != nil {
		t.Fatal(err)
	}
	if plain, err = svc.Register(ctx, fmt.Sprintf("state-plain-%d@example.com", stamp), testPassword); err != nil {
		t.Fatal(err)
	}
	permission, err := auth.EnsurePermission(ctx, db.DB, auth.PermissionManageUsers, "Disable and enable accounts")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.GrantPermissionToUser(ctx, db.DB, &manager, &permission); err != nil {
		t.Fatal(err)
	}
	return super, manager, plain
}

func userStatePath(id uint, action string) string {
	return fmt.Sprintf("/api/v1/auth/users/%d/%s", id, action)
}

func decodeUserState(t *testing.T, body []byte) (uint, bool) {
	t.Helper()
	var env dataBody[struct {
		ID       uint `json:"id"`
		Disabled bool `json:"disabled"`
	}]
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Data.ID, env.Data.Disabled
}

// runAccountStateBearer: the permission decides who may; a superuser's
// account is a superuser's to change; no one disables themselves; a
// disabled account's token fails at once, and its login is a 403.
func runAccountStateBearer(t *testing.T, db *database.DB) {
	t.Helper()
	app := newVerificationApp(t, db, config.AuthModeJWT, nil, config.EnvironmentTest)
	super, manager, plain := accountActors(t, app, db)
	plainPair := loginUser(t, app, plain.Email, testPassword)
	managerPair := loginUser(t, app, manager.Email, testPassword)
	superPair := loginUser(t, app, super.Email, testPassword)

	assertError(t, bearerPost(app, userStatePath(manager.ID, "disable"), plainPair.AccessToken, ""), http.StatusForbidden, "authorization")
	assertError(t, bearerPost(app, userStatePath(super.ID, "disable"), managerPair.AccessToken, ""), http.StatusForbidden, "authorization")
	assertError(t, bearerPost(app, userStatePath(manager.ID, "disable"), managerPair.AccessToken, ""), http.StatusConflict, "conflict")
	assertError(t, bearerPost(app, userStatePath(1<<30, "disable"), managerPair.AccessToken, ""), http.StatusNotFound, "not_found")
	assertError(t, bearerPost(app, userStatePath(plain.ID, "disable"), "", ""), http.StatusUnauthorized, "authentication")

	rec := bearerPost(app, userStatePath(plain.ID, "disable"), managerPair.AccessToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if id, disabled := decodeUserState(t, rec.Body.Bytes()); id != plain.ID || !disabled {
		t.Fatalf("disable body = %s", rec.Body.String())
	}
	// Its access token fails at once; its login is a 403.
	assertError(t, bearerRequest(app, http.MethodGet, "/api/v1/me", plainPair.AccessToken), http.StatusUnauthorized, "authentication")
	assertError(t, postJSON(t, app, "/api/v1/auth/login", `{"email":"`+plain.Email+`","password":"`+testPassword+`"}`), http.StatusForbidden, "authorization")
	assertError(t, postJSON(t, app, "/api/v1/auth/login", `{"email":"`+plain.Email+`","password":"wrong-horse"}`), http.StatusUnauthorized, "authentication")

	rec = bearerPost(app, userStatePath(plain.ID, "enable"), managerPair.AccessToken, "")
	if id, disabled := decodeUserState(t, rec.Body.Bytes()); rec.Code != http.StatusOK || id != plain.ID || disabled {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	loginUser(t, app, plain.Email, testPassword)

	// A superuser may change anyone, the manager too.
	if rec := bearerPost(app, userStatePath(manager.ID, "disable"), superPair.AccessToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("a superuser disabling the manager = %d %s", rec.Code, rec.Body.String())
	}
	assertError(t, bearerPost(app, userStatePath(plain.ID, "disable"), managerPair.AccessToken, ""), http.StatusUnauthorized, "authentication")
}

func TestAccountStateCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runAccountStateCookie(t, openSQLite(t))
}

// runAccountStateCookie: the same routes in cookie mode, behind CSRF.
func runAccountStateCookie(t *testing.T, db *database.DB) {
	t.Helper()
	app := newVerificationApp(t, db, config.AuthModeCookie, nil, config.EnvironmentTest)
	super, _, plain := accountActors(t, app, db)
	// The user's own cookie session, signed in before the disable.
	session := fetchCSRF(t, app)
	if rec := loginCookieUser(t, app, session, plain.Email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("the user's login = %d", rec.Code)
	}
	jar := fetchCSRF(t, app)
	if rec := loginCookieUser(t, app, jar, super.Email, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	if rec := doRequest(app, nil, http.MethodPost, userStatePath(plain.ID, "disable"), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("disable without the CSRF header = %d, want 403", rec.Code)
	}
	rec := doRequest(app, jar, http.MethodPost, userStatePath(plain.ID, "disable"), "")
	if id, disabled := decodeUserState(t, rec.Body.Bytes()); rec.Code != http.StatusOK || id != plain.ID || !disabled {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(app, session, http.MethodGet, "/api/v1/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the disabled user's cookie session on /me = %d, want 401", rec.Code)
	}
	other := fetchCSRF(t, app)
	if rec := loginCookieUser(t, app, other, plain.Email, testPassword); rec.Code != http.StatusForbidden {
		t.Fatalf("a disabled account's cookie login = %d, want 403", rec.Code)
	}
}

// sendHook runs before ahead of each message, then hands it to inner.
type sendHook struct {
	inner  auth.Mailer
	before func(auth.Message)
}

func (h sendHook) Send(ctx context.Context, msg auth.Message) error {
	h.before(msg)
	return h.inner.Send(ctx, msg)
}

// TestRegisterNeedsTheDisabledAtColumn holds the upgrade note to the ORM:
// registration writes users.disabled_at, so an unmigrated database refuses
// it (manifest account-state-schema, breaking).
func TestRegisterNeedsTheDisabledAtColumn(t *testing.T) {
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE users DROP COLUMN disabled_at").Error; err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	svc, err := auth.NewService(db.DB, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Register(context.Background(), "unmigrated@example.com", testPassword); err == nil {
		t.Fatal("Register on a users table without disabled_at succeeded: the manifest's breaking note is wrong")
	}
}

// TestRegisterPasswordErrors holds ErrInvalidCredentials' godoc to the code:
// Register refuses a password outside the policy with a PasswordPolicyError
// on "password", and a malformed email with ErrInvalidEmail.
func TestRegisterPasswordErrors(t *testing.T) {
	e := newVerificationEnv(t, openSQLite(t))
	for password, want := range map[string]error{"": auth.ErrPasswordRequired, "short": auth.ErrPasswordTooShort, strings.Repeat("x", 73): auth.ErrPasswordTooLong} {
		_, err := e.svc.Register(context.Background(), fmt.Sprintf("pw-%d@example.com", time.Now().UnixNano()), password)
		var policy auth.PasswordPolicyError
		if !errors.As(err, &policy) || policy.Field != "password" || !errors.Is(policy.Err, want) {
			t.Fatalf("Register(password of %d bytes) = %v, want a PasswordPolicyError on password of %v", len(password), err, want)
		}
	}
	if _, err := e.svc.Register(context.Background(), "not an address", testPassword); !errors.Is(err, auth.ErrInvalidEmail) {
		t.Fatalf("Register(malformed email) = %v, want ErrInvalidEmail", err)
	}
}
