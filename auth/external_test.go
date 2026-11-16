package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

// fakeProvider is an IdentityProvider double: it records each authorization
// request's challenge by state, and Exchange redeems the codes the test
// gave identities to, checking the PKCE verifier against the challenge as a
// provider does, and returning the request's nonce as an OIDC provider's ID
// token would.
type fakeProvider struct {
	name string
	mu   sync.Mutex
	// challenges by state, from AuthCodeURL.
	challenges map[string]string
	// codes: the identity each code redeems for, and its flow's state.
	codes map[string]fakeGrant
	// nonce, when set, replaces the request's nonce in the identity.
	nonce string
	// fail, when set, is Exchange's error.
	fail error
}

type fakeGrant struct {
	state    string
	identity auth.ExternalIdentity
}

func newFakeProvider(name string) *fakeProvider {
	return &fakeProvider{name: name, challenges: map[string]string{}, codes: map[string]fakeGrant{}}
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) AuthCodeURL(req auth.AuthorizationRequest) (string, error) {
	if req.CodeChallengeMethod != "S256" || req.Nonce == "" {
		return "", errors.New("fake: want S256 and a nonce")
	}
	p.mu.Lock()
	p.challenges[req.State] = req.CodeChallenge
	p.mu.Unlock()
	q := url.Values{"state": {req.State}, "nonce": {req.Nonce}, "code_challenge": {req.CodeChallenge}}
	return "https://idp.example.com/authorize?" + q.Encode(), nil
}

// grant is the provider authorizing the flow of authorizationURL for
// identity: the code its redirect carries.
func (p *fakeProvider) grant(t *testing.T, authorizationURL string, identity auth.ExternalIdentity) (code, state string) {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state = u.Query().Get("state")
	code = fmt.Sprintf("code-%d", time.Now().UnixNano())
	p.mu.Lock()
	p.codes[code] = fakeGrant{state: state, identity: identity}
	p.mu.Unlock()
	return code, state
}

func (p *fakeProvider) Exchange(_ context.Context, req auth.ExchangeRequest) (auth.ExternalIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return auth.ExternalIdentity{}, p.fail
	}
	g, ok := p.codes[req.Code]
	if !ok {
		return auth.ExternalIdentity{}, errors.New("fake: invalid_grant")
	}
	delete(p.codes, req.Code)
	sum := sha256.Sum256([]byte(req.CodeVerifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenges[g.state] {
		return auth.ExternalIdentity{}, errors.New("fake: PKCE verification failed")
	}
	id := g.identity
	id.Nonce = req.Nonce
	if p.nonce != "" {
		id.Nonce = p.nonce
	}
	return id, nil
}

// externalEnv is an app with the fake provider, in mode.
type externalEnv struct {
	app      *framework.App
	db       *database.DB
	provider *fakeProvider
	other    *fakeProvider
	cookie   bool
	// run makes this env's subjects its own (the integration databases
	// are shared between runs).
	run string
}

// sub is a subject of this env's.
func (e externalEnv) sub(name string) string { return e.run + "-" + name }

func newExternalEnv(t *testing.T, db *database.DB, mode config.AuthMode, opts ...framework.Option) externalEnv {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.Mode = mode
	p, other := newFakeProvider("fake"), newFakeProvider("other")
	all := append([]framework.Option{
		framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()),
		framework.WithAuthIdentityProviders(p, other),
	}, opts...)
	app, err := framework.New(all...)
	if err != nil {
		t.Fatal(err)
	}
	return externalEnv{app: app, db: db, provider: p, other: other, cookie: mode == config.AuthModeCookie, run: fmt.Sprintf("%s-%d", mode, time.Now().UnixNano())}
}

// browser is one browser: its cookies, and in Bearer mode its access token.
type browser struct {
	jar    *cookieJar
	access string
}

func (e externalEnv) browser(t *testing.T) *browser {
	t.Helper()
	if e.cookie {
		return &browser{jar: fetchCSRF(t, e.app)}
	}
	return &browser{jar: newCookieJar()}
}

func (e externalEnv) do(b *browser, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	b.jar.attach(req)
	if csrf := b.jar.value(auth.CSRFCookieName); csrf != "" {
		req.Header.Set(auth.CSRFHeaderName, csrf)
	}
	if b.access != "" {
		req.Header.Set("Authorization", "Bearer "+b.access)
	}
	e.app.Router().ServeHTTP(rec, req)
	b.jar.update(rec)
	return rec
}

// start begins a flow (login or link) and returns the authorization URL.
func (e externalEnv) start(t *testing.T, b *browser, provider, action, body string) string {
	t.Helper()
	rec := e.do(b, http.MethodPost, "/api/v1/auth/external/"+provider+"/"+action, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("start %s = %d %s", action, rec.Code, rec.Body.String())
	}
	var out dataBody[struct {
		URL string `json:"authorization_url"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Data.URL == "" {
		t.Fatalf("start body = %s", rec.Body.String())
	}
	return out.Data.URL
}

// callback is the provider's redirect back; it returns the redirect's
// location.
func (e externalEnv) callback(t *testing.T, b *browser, provider, query string) *url.URL {
	t.Helper()
	rec := e.do(b, http.MethodGet, "/api/v1/auth/external/"+provider+"/callback?"+query, "")
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if b.jar.value(auth.ExternalStateCookieName) != "" {
		t.Fatal("the callback left the state cookie")
	}
	return loc
}

// signIn runs a whole login flow for identity and returns where it lands.
func (e externalEnv) signIn(t *testing.T, b *browser, identity auth.ExternalIdentity, returnTo string) *url.URL {
	t.Helper()
	authURL := e.start(t, b, "fake", "login", `{"return_to":"`+returnTo+`"}`)
	code, state := e.provider.grant(t, authURL, identity)
	return e.callback(t, b, "fake", url.Values{"code": {code}, "state": {state}}.Encode())
}

// session completes a successful sign-in landing at loc: in Bearer mode it
// trades the code for tokens. It returns /me's user.
func (e externalEnv) session(t *testing.T, b *browser, loc *url.URL) meView {
	t.Helper()
	if errCode := loc.Query().Get("gombit_error"); errCode != "" {
		t.Fatalf("sign-in refused: %s", errCode)
	}
	if !e.cookie {
		code := loc.Query().Get("gombit_code")
		if code == "" {
			t.Fatalf("no gombit_code in %s", loc)
		}
		rec := e.do(b, http.MethodPost, "/api/v1/auth/external/exchange", `{"code":"`+code+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("exchange = %d %s", rec.Code, rec.Body.String())
		}
		var pair dataBody[tokenPair]
		if err := json.Unmarshal(rec.Body.Bytes(), &pair); err != nil {
			t.Fatal(err)
		}
		b.access = pair.Data.AccessToken
		if again := e.do(b, http.MethodPost, "/api/v1/auth/external/exchange", `{"code":"`+code+`"}`); again.Code != http.StatusUnprocessableEntity {
			t.Fatalf("a second exchange of the code = %d %s", again.Code, again.Body.String())
		}
	} else if loc.Query().Has("gombit_code") {
		t.Fatalf("cookie mode redirected with a code: %s", loc)
	}
	rec := e.do(b, http.MethodGet, "/api/v1/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/me = %d %s", rec.Code, rec.Body.String())
	}
	return decodeMe(t, rec)
}

func identity(subject, email string) auth.ExternalIdentity {
	return auth.ExternalIdentity{Subject: subject, Email: email, EmailVerified: true}
}

func uniqueEmail(name string) string {
	return fmt.Sprintf("%s-%d@example.com", name, time.Now().UnixNano())
}

func TestExternalIdentityBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runExternalIdentity(t, openSQLite(t), config.AuthModeJWT)
}

func TestExternalIdentityCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runExternalIdentity(t, openSQLite(t), config.AuthModeCookie)
}

func runExternalIdentity(t *testing.T, db *database.DB, mode config.AuthMode) {
	t.Helper()
	e := newExternalEnv(t, db, mode)
	ctx := context.Background()

	t.Run("providers are listed", func(t *testing.T) {
		rec := e.do(e.browser(t), http.MethodGet, "/api/v1/auth/external", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `[{"name":"fake"},{"name":"other"}]`) {
			t.Fatalf("providers = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unlinked identity with a verified email makes an account and signs in", func(t *testing.T) {
		b := e.browser(t)
		email := uniqueEmail("ext-signup")
		authURL := e.start(t, b, "fake", "login", `{"return_to":"/welcome?tab=1"}`)
		c := b.jar.cookies[auth.ExternalStateCookieName]
		if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/api/v1/auth/external" || c.MaxAge != 600 {
			t.Fatalf("state cookie = %+v", c)
		}
		if strings.Contains(c.Value, "nonce") || strings.Contains(c.Value, "welcome") {
			t.Fatal("the state cookie is not sealed")
		}
		code, st := e.provider.grant(t, authURL, identity(e.sub("sub-signup"), strings.ToUpper(email)))
		loc := e.callback(t, b, "fake", url.Values{"code": {code}, "state": {st}}.Encode())
		if loc.Path != "/welcome" || loc.Query().Get("tab") != "1" || loc.Host != "" {
			t.Fatalf("landed at %s", loc)
		}
		me := e.session(t, b, loc)
		if me.Email != email || !me.EmailVerified {
			t.Fatalf("/me = %+v", me)
		}
		var user auth.User
		if err := db.DB.Where("email = ?", email).First(&user).Error; err != nil {
			t.Fatal(err)
		}
		if user.HasUsablePassword() {
			t.Fatal("an external account has a usable password")
		}
		// No password signs in to it, not even its marker.
		rec := e.do(e.browser(t), http.MethodPost, "/api/v1/auth/login", `{"email":"`+email+`","password":"`+user.PasswordHash[:60]+`"}`)
		assertError(t, rec, http.StatusUnauthorized, "authentication")
		var events []auth.AuditEvent
		if err := db.DB.Where("target_user_id = ? AND action = ?", user.ID, auth.AuditRegistered).Find(&events).Error; err != nil || len(events) != 1 || !strings.Contains(events[0].Details, `"provider":"fake"`) {
			t.Fatalf("registration audit = %+v, %v", events, err)
		}

		// The same identity signs in to the same account, whatever email the
		// provider has for it now.
		b2 := e.browser(t)
		me2 := e.session(t, b2, e.signIn(t, b2, identity(e.sub("sub-signup"), uniqueEmail("renamed")), "/"))
		if me2.ID != me.ID {
			t.Fatalf("second sign-in user = %d, want %d", me2.ID, me.ID)
		}
		var links []auth.IdentityLink
		if err := db.DB.Where("user_id = ?", user.ID).Find(&links).Error; err != nil || len(links) != 1 || links[0].Email == email || links[0].LastUsedAt == nil {
			t.Fatalf("links = %+v, %v", links, err)
		}
	})

	t.Run("subjects are compared exactly", func(t *testing.T) {
		// Distinct subjects at the provider are distinct users, whatever the
		// database's collation (MySQL's ignores case and trailing spaces).
		b1, b2, b3 := e.browser(t), e.browser(t), e.browser(t)
		u1 := e.session(t, b1, e.signIn(t, b1, identity(e.sub("CaseSub"), uniqueEmail("case-1")), "/"))
		u2 := e.session(t, b2, e.signIn(t, b2, identity(e.sub("casesub"), uniqueEmail("case-2")), "/"))
		u3 := e.session(t, b3, e.signIn(t, b3, identity(e.sub("CaseSub")+" ", uniqueEmail("case-3")), "/"))
		if u1.ID == u2.ID || u1.ID == u3.ID || u2.ID == u3.ID {
			t.Fatalf("users = %d, %d, %d: subjects differing in case or a trailing space shared an account", u1.ID, u2.ID, u3.ID)
		}
	})

	t.Run("an identity whose email is an account's is refused, not linked", func(t *testing.T) {
		email := uniqueEmail("ext-existing")
		e.register(t, email, testPassword)
		b := e.browser(t)
		loc := e.signIn(t, b, identity(e.sub("sub-existing"), email), "/x")
		if loc.Path != "/x" || loc.Query().Get("gombit_error") != "account_exists" || loc.Query().Has("gombit_code") {
			t.Fatalf("landed at %s", loc)
		}
		var n int64
		db.DB.Model(&auth.IdentityLink{}).Where("provider = ? AND subject = ?", "fake", e.sub("sub-existing")).Count(&n)
		if n != 0 || b.jar.value(auth.AccessCookieName) != "" {
			t.Fatalf("links = %d, access cookie %q", n, b.jar.value(auth.AccessCookieName))
		}
	})

	t.Run("no account without a verified email", func(t *testing.T) {
		for name, id := range map[string]auth.ExternalIdentity{
			"unverified": {Subject: e.sub("sub-unverified"), Email: uniqueEmail("unverified")},
			"no email":   {Subject: e.sub("sub-noemail"), EmailVerified: true},
		} {
			loc := e.signIn(t, e.browser(t), id, "/")
			if loc.Query().Get("gombit_error") != "email_unverified" {
				t.Fatalf("%s: landed at %s", name, loc)
			}
		}
		var n int64
		db.DB.Model(&auth.IdentityLink{}).Where("subject IN ?", []string{e.sub("sub-unverified"), e.sub("sub-noemail")}).Count(&n)
		if n != 0 {
			t.Fatalf("links = %d", n)
		}
	})

	t.Run("the identity must be the flow's", func(t *testing.T) {
		e.provider.nonce = "another-nonce"
		loc := e.signIn(t, e.browser(t), identity(e.sub("sub-nonce"), uniqueEmail("nonce")), "/")
		e.provider.nonce = ""
		if loc.Query().Get("gombit_error") != "invalid_identity" {
			t.Fatalf("a foreign nonce landed at %s", loc)
		}
		loc = e.signIn(t, e.browser(t), auth.ExternalIdentity{Email: uniqueEmail("nosub"), EmailVerified: true}, "/")
		if loc.Query().Get("gombit_error") != "invalid_identity" {
			t.Fatalf("no subject landed at %s", loc)
		}
	})

	t.Run("the provider's refusals", func(t *testing.T) {
		b := e.browser(t)
		authURL := e.start(t, b, "fake", "login", "")
		_, st := e.provider.grant(t, authURL, identity("x", "x@example.com"))
		if loc := e.callback(t, b, "fake", url.Values{"error": {"access_denied"}, "state": {st}}.Encode()); loc.String() != "/?gombit_error=access_denied" {
			t.Fatalf("declined landed at %s", loc)
		}
		e.provider.fail = errors.New("boom")
		loc := e.signIn(t, e.browser(t), identity(e.sub("sub-fail"), uniqueEmail("fail")), "/")
		e.provider.fail = nil
		if loc.Query().Get("gombit_error") != "provider_error" {
			t.Fatalf("an exchange failure landed at %s", loc)
		}
	})

	t.Run("the state is checked", func(t *testing.T) {
		b := e.browser(t)
		authURL := e.start(t, b, "fake", "login", "")
		code, st := e.provider.grant(t, authURL, identity(e.sub("sub-state"), uniqueEmail("state")))
		sealed := b.jar.value(auth.ExternalStateCookieName)
		for name, tc := range map[string]struct{ provider, query, cookie string }{
			"another state":    {"fake", url.Values{"code": {code}, "state": {st + "x"}}.Encode(), sealed},
			"no state":         {"fake", url.Values{"code": {code}}.Encode(), sealed},
			"no cookie":        {"fake", url.Values{"code": {code}, "state": {st}}.Encode(), ""},
			"altered cookie":   {"fake", url.Values{"code": {code}, "state": {st}}.Encode(), sealed[:len(sealed)-2] + "AA"},
			"another provider": {"other", url.Values{"code": {code}, "state": {st}}.Encode(), sealed},
		} {
			other := &browser{jar: newCookieJar()}
			if tc.cookie != "" {
				other.jar.cookies[auth.ExternalStateCookieName] = &http.Cookie{Name: auth.ExternalStateCookieName, Value: tc.cookie}
			}
			rec := e.do(other, http.MethodGet, "/api/v1/auth/external/"+tc.provider+"/callback?"+tc.query, "")
			assertError(t, rec, http.StatusUnprocessableEntity, "validation_error")
			if tc.cookie != "" && other.jar.value(auth.ExternalStateCookieName) != "" {
				t.Fatalf("%s: the refusal kept the state cookie", name)
			}
		}
		// The flow's own browser still completes it, once.
		loc := e.callback(t, b, "fake", url.Values{"code": {code}, "state": {st}}.Encode())
		e.session(t, b, loc)
		rec := e.do(&browser{jar: &cookieJar{cookies: map[string]*http.Cookie{auth.ExternalStateCookieName: {Name: auth.ExternalStateCookieName, Value: sealed}}}},
			http.MethodGet, "/api/v1/auth/external/fake/callback?"+url.Values{"code": {code}, "state": {st}}.Encode(), "")
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "gombit_error=provider_error") {
			t.Fatalf("a replayed callback = %d %s", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("return_to is a path on this site", func(t *testing.T) {
		b := e.browser(t)
		for _, bad := range []string{"https://evil.example.com/", "//evil.example.com", `/\\evil.example.com`, "evil", "/a#frag"} {
			body, _ := json.Marshal(map[string]string{"return_to": bad})
			rec := e.do(b, http.MethodPost, "/api/v1/auth/external/fake/login", string(body))
			assertError(t, rec, http.StatusUnprocessableEntity, "validation_error")
		}
		if rec := e.do(b, http.MethodPost, "/api/v1/auth/external/nope/login", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown provider = %d", rec.Code)
		}
	})

	t.Run("a disabled account does not sign in", func(t *testing.T) {
		email := uniqueEmail("ext-disabled")
		b := e.browser(t)
		me := e.session(t, b, e.signIn(t, b, identity(e.sub("sub-disabled"), email), "/"))
		if err := db.DB.Model(&auth.User{}).Where("id = ?", me.ID).Update("disabled_at", time.Now()).Error; err != nil {
			t.Fatal(err)
		}
		loc := e.signIn(t, e.browser(t), identity(e.sub("sub-disabled"), email), "/")
		if loc.Query().Get("gombit_error") != "account_disabled" {
			t.Fatalf("landed at %s", loc)
		}
	})

	t.Run("linking, then signing in with the link", func(t *testing.T) {
		email := uniqueEmail("ext-link")
		e.register(t, email, testPassword)
		b := e.passwordSession(t, email)
		// The password is required, and checked.
		rec := e.do(b, http.MethodPost, "/api/v1/auth/external/fake/link", `{"return_to":"/settings"}`)
		assertError(t, rec, http.StatusUnprocessableEntity, "validation_error")
		rec = e.do(b, http.MethodPost, "/api/v1/auth/external/fake/link", `{"current_password":"wrong-one"}`)
		assertError(t, rec, http.StatusUnprocessableEntity, "validation_error")
		authURL := e.start(t, b, "fake", "link", `{"return_to":"/settings","current_password":"`+testPassword+`"}`)
		// The provider's email is not the account's: linking needs none.
		code, st := e.provider.grant(t, authURL, auth.ExternalIdentity{Subject: e.sub("sub-link"), Email: uniqueEmail("elsewhere")})
		loc := e.callback(t, b, "fake", url.Values{"code": {code}, "state": {st}}.Encode())
		if loc.String() != "/settings?gombit_linked=fake" {
			t.Fatalf("link landed at %s", loc)
		}
		rec = e.do(b, http.MethodGet, "/api/v1/auth/external/identities", "")
		var links dataBody[[]struct {
			ID       uint   `json:"id"`
			Provider string `json:"provider"`
			Subject  string `json:"subject"`
		}]
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &links) != nil || len(links.Data) != 1 || links.Data[0].Subject != e.sub("sub-link") || links.Data[0].Provider != "fake" {
			t.Fatalf("identities = %d %s", rec.Code, rec.Body.String())
		}
		b2 := e.browser(t)
		if me := e.session(t, b2, e.signIn(t, b2, identity(e.sub("sub-link"), uniqueEmail("whatever")), "/")); me.Email != email {
			t.Fatalf("sign-in with the link = %+v", me)
		}

		// Another account cannot link it.
		other := uniqueEmail("ext-link-other")
		e.register(t, other, testPassword)
		ob := e.passwordSession(t, other)
		authURL = e.start(t, ob, "fake", "link", `{"current_password":"`+testPassword+`"}`)
		code, st = e.provider.grant(t, authURL, identity(e.sub("sub-link"), other))
		if loc := e.callback(t, ob, "fake", url.Values{"code": {code}, "state": {st}}.Encode()); loc.Query().Get("gombit_error") != "identity_linked_elsewhere" {
			t.Fatalf("linking another's identity landed at %s", loc)
		}

		// A link of a session that ended meanwhile is refused.
		authURL = e.start(t, ob, "fake", "link", `{"current_password":"`+testPassword+`"}`)
		code, st = e.provider.grant(t, authURL, identity(e.sub("sub-link-ended"), other))
		if rec := e.do(ob, http.MethodDelete, "/api/v1/auth/sessions?scope=all", ""); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
			t.Fatalf("revoke sessions = %d %s", rec.Code, rec.Body.String())
		}
		if loc := e.callback(t, ob, "fake", url.Values{"code": {code}, "state": {st}}.Encode()); loc.Query().Get("gombit_error") != "session_ended" {
			t.Fatalf("a link after the session ended landed at %s", loc)
		}

		// Unlinking: another's link is not found; the account's goes (it
		// has a password to sign in with).
		rec = e.do(e.passwordSession(t, other), http.MethodDelete, fmt.Sprintf("/api/v1/auth/external/identities/%d", links.Data[0].ID), "")
		assertError(t, rec, http.StatusNotFound, "not_found")
		if rec := e.do(b, http.MethodDelete, fmt.Sprintf("/api/v1/auth/external/identities/%d", links.Data[0].ID), ""); rec.Code != http.StatusNoContent {
			t.Fatalf("unlink = %d %s", rec.Code, rec.Body.String())
		}
		var events []auth.AuditEvent
		db.DB.Where("action IN ?", []auth.AuditAction{auth.AuditIdentityLinked, auth.AuditIdentityUnlinked}).Where("details LIKE ?", `%"fake"%`).Find(&events)
		if len(events) < 2 {
			t.Fatalf("link audit = %+v", events)
		}
	})

	t.Run("an account without a password keeps its last link", func(t *testing.T) {
		b := e.browser(t)
		e.session(t, b, e.signIn(t, b, identity(e.sub("sub-only"), uniqueEmail("ext-only")), "/"))
		rec := e.do(b, http.MethodGet, "/api/v1/auth/external/identities", "")
		var links dataBody[[]struct {
			ID uint `json:"id"`
		}]
		if json.Unmarshal(rec.Body.Bytes(), &links) != nil || len(links.Data) != 1 {
			t.Fatalf("identities = %s", rec.Body.String())
		}
		rec = e.do(b, http.MethodDelete, fmt.Sprintf("/api/v1/auth/external/identities/%d", links.Data[0].ID), "")
		assertError(t, rec, http.StatusConflict, "conflict")
		// It links another without a password, then may drop the first.
		authURL := e.start(t, b, "other", "link", "")
		code, st := e.other.grant(t, authURL, auth.ExternalIdentity{Subject: fmt.Sprintf("other-%d", time.Now().UnixNano())})
		if loc := e.callback(t, b, "other", url.Values{"code": {code}, "state": {st}}.Encode()); loc.Query().Get("gombit_linked") != "other" {
			t.Fatalf("second link landed at %s", loc)
		}
		if rec := e.do(b, http.MethodDelete, fmt.Sprintf("/api/v1/auth/external/identities/%d", links.Data[0].ID), ""); rec.Code != http.StatusNoContent {
			t.Fatalf("unlink with another left = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an account with multi-factor authentication signs in with its password", func(t *testing.T) {
		email := uniqueEmail("ext-mfa")
		e.register(t, email, testPassword)
		b := e.passwordSession(t, email)
		authURL := e.start(t, b, "fake", "link", `{"current_password":"`+testPassword+`"}`)
		code, st := e.provider.grant(t, authURL, identity(e.sub("sub-mfa"), email))
		if loc := e.callback(t, b, "fake", url.Values{"code": {code}, "state": {st}}.Encode()); loc.Query().Get("gombit_linked") != "fake" {
			t.Fatalf("link landed at %s", loc)
		}
		svc, err := auth.NewService(db.DB, e.app.Config())
		if err != nil {
			t.Fatal(err)
		}
		var user auth.User
		db.DB.Where("email = ?", email).First(&user)
		enrollment, err := svc.BeginTOTPEnrollment(ctx, user.ID, testPassword)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ConfirmTOTPEnrollment(ctx, user.ID, auth.TOTPCode(enrollment.Secret, time.Now())); err != nil {
			t.Fatal(err)
		}
		if loc := e.signIn(t, e.browser(t), identity(e.sub("sub-mfa"), email), "/"); loc.Query().Get("gombit_error") != "mfa_enabled" {
			t.Fatalf("an MFA account's external sign-in landed at %s", loc)
		}
		b2 := e.passwordSessionMFA(t, email, auth.TOTPCode(enrollment.Secret, time.Now().Add(30*time.Second)))
		rec := e.do(b2, http.MethodPost, "/api/v1/auth/external/other/link", `{"current_password":"`+testPassword+`"}`)
		assertError(t, rec, http.StatusConflict, "conflict")
	})
}

// register makes a password account.
func (e externalEnv) register(t *testing.T, email, password string) {
	t.Helper()
	rec := e.do(e.browser(t), http.MethodPost, "/api/v1/auth/register", `{"email":"`+email+`","password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
}

// passwordSession signs email in with its password.
func (e externalEnv) passwordSession(t *testing.T, email string) *browser {
	t.Helper()
	return e.passwordSessionMFA(t, email, "")
}

func (e externalEnv) passwordSessionMFA(t *testing.T, email, code string) *browser {
	t.Helper()
	b := e.browser(t)
	body := `{"email":"` + email + `","password":"` + testPassword + `"`
	if code != "" {
		body += `,"mfa_code":"` + code + `"`
	}
	rec := e.do(b, http.MethodPost, "/api/v1/auth/login", body+"}")
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	if !e.cookie {
		var pair dataBody[tokenPair]
		if err := json.Unmarshal(rec.Body.Bytes(), &pair); err != nil {
			t.Fatal(err)
		}
		b.access = pair.Data.AccessToken
	}
	return b
}

// TestExternalRoutesNeedAProvider: without one, nothing is mounted.
func TestExternalRoutesNeedAProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	for _, path := range []string{"/api/v1/auth/external", "/api/v1/auth/external/google/callback"} {
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if _, ok := app.API().OpenAPI().Paths["/api/v1/auth/external"]; ok {
		t.Fatal("the OpenAPI document lists the external routes")
	}
}

// TestExternalRoutesByMode: the exchange is Bearer mode's only.
func TestExternalRoutesByMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := newExternalEnv(t, openSQLite(t), config.AuthModeJWT)
	cookie := newExternalEnv(t, openSQLite(t), config.AuthModeCookie)
	paths := bearer.app.API().OpenAPI().Paths
	for _, p := range []string{"/api/v1/auth/external", "/api/v1/auth/external/{provider}/login", "/api/v1/auth/external/{provider}/link", "/api/v1/auth/external/{provider}/callback", "/api/v1/auth/external/exchange", "/api/v1/auth/external/identities", "/api/v1/auth/external/identities/{id}"} {
		if paths[p] == nil {
			t.Errorf("Bearer mode lacks %s", p)
		}
	}
	if cookie.app.API().OpenAPI().Paths["/api/v1/auth/external/exchange"] != nil {
		t.Error("cookie mode mounts the exchange")
	}
}
