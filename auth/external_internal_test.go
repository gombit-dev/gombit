package auth

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"golang.org/x/crypto/bcrypt"
)

type namedProvider struct{ name string }

func (p namedProvider) Name() string { return p.name }
func (p namedProvider) AuthCodeURL(req AuthorizationRequest) (string, error) {
	return "https://idp.example.com/authorize?state=" + url.QueryEscape(req.State), nil
}
func (p namedProvider) Exchange(context.Context, ExchangeRequest) (ExternalIdentity, error) {
	return ExternalIdentity{}, errors.New("unused")
}

type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func newExternalTestService(t *testing.T, opts ...Option) (*Service, *fixedClock) {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "ext.db") + "?_fk=1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = strings.Repeat("j", 32)
	cfg.Auth.BcryptCost = bcrypt.MinCost
	svc, err := NewService(db.DB, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	svc.clock = clock
	return svc, clock
}

// TestIdentityProviderNames: names are checked, and distinct.
func TestIdentityProviderNames(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "names.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = strings.Repeat("j", 32)
	for name, providers := range map[string][]IdentityProvider{
		"empty":     {namedProvider{""}},
		"uppercase": {namedProvider{"Google"}},
		"too long":  {namedProvider{strings.Repeat("a", 33)}},
		"reserved":  {namedProvider{"identities"}},
		"twice":     {namedProvider{"google"}, namedProvider{"google"}},
		"nil":       {nil},
	} {
		if _, err := NewService(db.DB, cfg, WithIdentityProviders(providers...)); err == nil {
			t.Errorf("%s: NewService accepted the providers", name)
		}
	}
	svc, err := NewService(db.DB, cfg, WithIdentityProviders(namedProvider{"google"}, namedProvider{"okta-2"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.IdentityProviders(); len(got) != 2 || got[0] != "google" || got[1] != "okta-2" {
		t.Fatalf("IdentityProviders = %v", got)
	}
}

// TestExternalStateExpires: a flow's state opens for its provider and state
// until externalFlowTTL has passed, and not after.
func TestExternalStateExpires(t *testing.T) {
	svc, clock := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
	start, err := svc.beginExternal("idp", externalState{Mode: externalModeLogin})
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.openExternalState("idp", start.Sealed, stateOf(t, start))
	if err != nil || st.ReturnTo != "/" || st.Verifier == "" || st.Nonce == "" {
		t.Fatalf("open = %+v, %v", st, err)
	}
	clock.now = clock.now.Add(externalFlowTTL - time.Second)
	if _, err := svc.openExternalState("idp", start.Sealed, st.State); err != nil {
		t.Fatalf("open before expiry = %v", err)
	}
	clock.now = clock.now.Add(time.Second)
	if _, err := svc.openExternalState("idp", start.Sealed, st.State); !errors.Is(err, errExternalState) {
		t.Fatalf("open at expiry = %v, want errExternalState", err)
	}
}

// stateOf is the state a flow's authorization URL carries, as the
// provider echoes it.
func stateOf(t *testing.T, start externalStart) string {
	t.Helper()
	u, err := url.Parse(start.URL)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("authorization URL %q: %v", start.URL, err)
	}
	return u.Query().Get("state")
}

// TestExternalCodeExpires: a Bearer exchange code works once, within
// externalCodeTTL.
func TestExternalCodeExpires(t *testing.T) {
	ctx := context.Background()
	svc, clock := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
	id := ExternalIdentity{Subject: "s1", Email: "ext-code@example.com", EmailVerified: true}
	first, err := svc.externalLogin(ctx, "idp", id, true)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(externalCodeTTL)
	if _, _, err := svc.exchangeExternalCode(ctx, first.Code, first.Binding); !errors.Is(err, errInvalidOneTimeToken) {
		t.Fatalf("an expired code = %v, want errInvalidOneTimeToken", err)
	}
	second, err := svc.externalLogin(ctx, "idp", id, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.User.ID != first.User.ID {
		t.Fatalf("second sign-in user = %d, want %d", second.User.ID, first.User.ID)
	}
	clock.now = clock.now.Add(externalCodeTTL - time.Second)
	if _, pair, err := svc.exchangeExternalCode(ctx, second.Code, second.Binding); err != nil || pair.AccessToken == "" {
		t.Fatalf("exchange = %v", err)
	}
	if _, _, err := svc.exchangeExternalCode(ctx, second.Code, second.Binding); !errors.Is(err, errInvalidOneTimeToken) {
		t.Fatalf("a used code = %v, want errInvalidOneTimeToken", err)
	}
}

// TestExternalSignupOff: without signup, an identity no user linked makes
// no account.
func TestExternalSignupOff(t *testing.T) {
	ctx := context.Background()
	svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}), WithoutExternalSignup())
	_, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s1", Email: "ext-off@example.com", EmailVerified: true}, false)
	if !errors.Is(err, errExternalSignupDisabled) || externalErrorCode(err) != "signup_disabled" {
		t.Fatalf("signup = %v", err)
	}
	var n int64
	svc.db.Model(&User{}).Count(&n)
	if n != 0 {
		t.Fatalf("users = %d", n)
	}
}

// TestCleanReturnTo: only a path on this site.
func TestCleanReturnTo(t *testing.T) {
	for raw, want := range map[string]string{"": "/", "/": "/", "/a/b?c=d": "/a/b?c=d"} {
		if got, err := cleanReturnTo(raw); err != nil || got != want {
			t.Errorf("cleanReturnTo(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"a", "//x", "/\\x", "/a\\b", "/a#b", "/a\nb", "https://x/", "/%zz", "/a?b=%zz%", "/" + strings.Repeat("a", 2048)} {
		if _, err := cleanReturnTo(raw); !errors.Is(err, errInvalidReturnTo) {
			t.Errorf("cleanReturnTo(%q) = %v, want errInvalidReturnTo", raw, err)
		}
	}
}

// TestExternalLoginDropsAStaleLink: an identity whose user was deleted is
// no user's again: it signs up anew.
func TestExternalLoginDropsAStaleLink(t *testing.T) {
	ctx := context.Background()
	svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
	id := ExternalIdentity{Subject: "s1", Email: "ext-stale@example.com", EmailVerified: true}
	first, err := svc.externalLogin(ctx, "idp", id, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.db.Delete(&User{}, first.User.ID).Error; err != nil {
		t.Fatal(err)
	}
	second, err := svc.externalLogin(ctx, "idp", id, false)
	if err != nil {
		t.Fatalf("sign-in after the user was deleted = %v", err)
	}
	if second.User.ID == first.User.ID || second.Pair.AccessToken == "" {
		t.Fatalf("second sign-in = user %d, want a new account", second.User.ID)
	}
	var links []IdentityLink
	if err := svc.db.Where("provider = ?", "idp").Find(&links).Error; err != nil || len(links) != 1 || links[0].UserID != second.User.ID {
		t.Fatalf("links = %+v, %v", links, err)
	}
}

// TestExternalCodeNeedsItsBinding: a code exchanges only with its own
// browser binding.
func TestExternalCodeNeedsItsBinding(t *testing.T) {
	ctx := context.Background()
	svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
	res, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s1", Email: "ext-bind@example.com", EmailVerified: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s2", Email: "ext-bind-2@example.com", EmailVerified: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]externalBinding{"none": {}, "another flow's": other.Binding, "made up": {Secret: "x", Provider: "idp"}} {
		if _, _, err := svc.exchangeExternalCode(ctx, res.Code, b); !errors.Is(err, errInvalidOneTimeToken) {
			t.Fatalf("%s binding = %v, want errInvalidOneTimeToken", name, err)
		}
	}
	if _, pair, err := svc.exchangeExternalCode(ctx, res.Code, res.Binding); err != nil || pair.AccessToken == "" {
		t.Fatalf("own binding = %v", err)
	}
	var events []AuditEvent
	svc.db.Where("action = ? AND target_user_id = ?", AuditLoginSucceeded, res.User.ID).Find(&events)
	if len(events) != 1 || !strings.Contains(events[0].Details, `"provider":"idp"`) {
		t.Fatalf("login audit = %+v", events)
	}
}

// TestExternalCodeVoidedWithTheSessions: a disable, a sign-out everywhere,
// a password reset each void a code not yet exchanged.
func TestExternalCodeVoidedWithTheSessions(t *testing.T) {
	ctx := context.Background()
	for name, end := range map[string]func(svc *Service, user User) error{
		"disable": func(svc *Service, user User) error {
			_, err := svc.DisableUser(ctx, user.ID)
			return err
		},
		"sign out everywhere": func(svc *Service, user User) error { return svc.RevokeAllSessions(ctx, user.ID) },
	} {
		svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
		res, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s1", Email: "ext-void@example.com", EmailVerified: true}, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := end(svc, res.User); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, _, err := svc.exchangeExternalCode(ctx, res.Code, res.Binding); !errors.Is(err, errInvalidOneTimeToken) {
			t.Fatalf("%s: exchange = %v, want errInvalidOneTimeToken (a 422)", name, err)
		}
	}
}

// TestPasswordResetRemovesLinks: a reset that ends the sessions removes the
// linked identities; one that keeps them keeps the links.
func TestPasswordResetRemovesLinks(t *testing.T) {
	ctx := context.Background()
	for _, keep := range []bool{false, true} {
		svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
		svc.cfg.PasswordResetKeepSessions = keep
		mail := &recordingMailer{}
		svc.mailer = mail
		res, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s1", Email: "ext-reset@example.com", EmailVerified: true}, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.RequestPasswordReset(ctx, res.User.Email); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ResetPassword(ctx, mail.last.Token, "a-new-password"); err != nil {
			t.Fatal(err)
		}
		var n int64
		svc.db.Model(&IdentityLink{}).Where("user_id = ?", res.User.ID).Count(&n)
		if want := map[bool]int64{false: 0, true: 1}[keep]; n != want {
			t.Fatalf("keep sessions %v: links = %d, want %d", keep, n, want)
		}
	}
}

type recordingMailer struct{ last Message }

func (m *recordingMailer) Send(_ context.Context, msg Message) error {
	m.last = msg
	return nil
}

// TestExternalCookiesSecureInProduction: in production the flow's cookies
// are Secure in either mode, whatever GOMBIT_COOKIE_SECURE says.
func TestExternalCookiesSecureInProduction(t *testing.T) {
	svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
	svc.cfg.CookieSecure = false
	if svc.externalStateCookie("v", 1).Secure || svc.externalCodeCookie("v", 1).Secure {
		t.Fatal("Secure outside production without CookieSecure")
	}
	svc.production = true
	if !svc.externalStateCookie("v", 1).Secure || !svc.externalCodeCookie("v", 1).Secure {
		t.Fatal("not Secure in production")
	}
}

// TestCheckIdentityNonce: a declared ID token provider must return the
// flow's nonce; any adapter's returned nonce must be the flow's.
func TestCheckIdentityNonce(t *testing.T) {
	id := func(nonce string) ExternalIdentity { return ExternalIdentity{Subject: "s", Nonce: nonce} }
	for _, tc := range []struct {
		nonce   string
		idToken bool
		ok      bool
	}{{"n", true, true}, {"", true, false}, {"x", true, false}, {"", false, true}, {"n", false, true}, {"x", false, false}} {
		if err := checkIdentity(id(tc.nonce), "n", tc.idToken); (err == nil) != tc.ok {
			t.Errorf("nonce %q, ID tokens %v: %v", tc.nonce, tc.idToken, err)
		}
	}
}

// TestExternalCodeVoidedWithTheOtherSessions: ending the other sessions (a
// password change does) voids a code not yet exchanged: another session
// about to start.
func TestExternalCodeVoidedWithTheOtherSessions(t *testing.T) {
	ctx := context.Background()
	svc, _ := newExternalTestService(t, WithIdentityProviders(namedProvider{"idp"}))
	first, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s1", Email: "ext-others@example.com", EmailVerified: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	var keep RefreshToken
	if err := svc.db.Where("user_id = ?", first.User.ID).Order("id DESC").First(&keep).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := svc.externalLogin(ctx, "idp", ExternalIdentity{Subject: "s1", Email: "ext-others@example.com", EmailVerified: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.revokeOtherSessions(ctx, first.User.ID, keep.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.exchangeExternalCode(ctx, pending.Code, pending.Binding); !errors.Is(err, errInvalidOneTimeToken) {
		t.Fatalf("exchange after the other sessions ended = %v, want errInvalidOneTimeToken", err)
	}
}
