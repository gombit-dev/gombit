package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/oidc"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

const (
	clientID     = "gombit-app"
	clientSecret = "s3cret"
	redirectURL  = "http://app.example.com/api/v1/auth/external/test/callback"
)

// issuer is an OpenID Connect provider: discovery, keys, an authorization
// endpoint that grants at once, and a token endpoint that checks the client
// and the PKCE verifier, and signs the ID token.
type issuer struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer
	mu     sync.Mutex
	grants map[string]grant
	// claims is what the next authorization's ID token says, besides iss,
	// aud, exp, iat and nonce.
	claims map[string]any
	// tamper alters the next ID token's standard claims.
	tamper func(*jwt.Claims, *string)
}

type grant struct {
	challenge, nonce string
	claims           map[string]any
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	iss := &issuer{t: t, key: key, signer: signer, grants: map[string]grant{}, claims: map[string]any{"sub": "user-1", "email": "Ada@Example.com", "email_verified": true}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                iss.srv.URL,
			"authorization_endpoint":                iss.srv.URL + "/authorize",
			"token_endpoint":                        iss.srv.URL + "/token",
			"jwks_uri":                              iss.srv.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != clientID || q.Get("redirect_uri") != redirectURL || q.Get("response_type") != "code" ||
			q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" ||
			!strings.Contains(q.Get("scope"), "openid") || !strings.Contains(q.Get("scope"), "email") {
			http.Error(w, "bad authorization request: "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		iss.mu.Lock()
		iss.grants[code] = grant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), claims: iss.claims}
		iss.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		id, secret, _ := r.BasicAuth()
		if err := r.ParseForm(); err != nil || id != clientID || secret != clientSecret || r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		iss.mu.Lock()
		g, ok := iss.grants[r.Form.Get("code")]
		delete(iss.grants, r.Form.Get("code"))
		iss.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		std := jwt.Claims{Issuer: iss.srv.URL, Audience: jwt.Audience{clientID}, Expiry: jwt.NewNumericDate(time.Now().Add(time.Minute)), IssuedAt: jwt.NewNumericDate(time.Now())}
		nonce := g.nonce
		if iss.tamper != nil {
			iss.tamper(&std, &nonce)
		}
		extra := map[string]any{"nonce": nonce}
		for k, v := range g.claims {
			extra[k] = v
		}
		raw, err := jwt.Signed(iss.signer).Claims(std).Claims(extra).Serialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60, "id_token": raw})
	})
	iss.srv = httptest.NewServer(mux)
	t.Cleanup(iss.srv.Close)
	return iss
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (iss *issuer) provider(t *testing.T) *oidc.Provider {
	t.Helper()
	p, err := oidc.New(context.Background(), oidc.Config{Name: "test", Issuer: iss.srv.URL, ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// authorize sends the browser to the provider's authorization URL, which
// grants at once: the code and state it redirects back with.
func (iss *issuer) authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize = %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}

func pkce(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestProviderExchange(t *testing.T) {
	iss := newIssuer(t)
	p := iss.provider(t)
	ctx := context.Background()
	run := func(verifier, nonce string) (auth.ExternalIdentity, error) {
		authURL, err := p.AuthCodeURL(auth.AuthorizationRequest{State: "st", Nonce: nonce, CodeChallenge: pkce(verifier), CodeChallengeMethod: "S256"})
		if err != nil {
			t.Fatal(err)
		}
		code, state := iss.authorize(t, authURL)
		if state != "st" {
			t.Fatalf("state = %q", state)
		}
		return p.Exchange(ctx, auth.ExchangeRequest{Code: code, CodeVerifier: verifier, Nonce: nonce})
	}

	id, err := run("verifier-1", "nonce-1")
	if err != nil || id.Subject != "user-1" || id.Email != "Ada@Example.com" || !id.EmailVerified || id.Nonce != "nonce-1" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if p.Name() != "test" {
		t.Fatalf("Name = %q", p.Name())
	}

	iss.claims = map[string]any{"sub": "user-2", "email": "b@example.com", "email_verified": "true"}
	if id, err := run("verifier-2", "nonce-2"); err != nil || !id.EmailVerified {
		t.Fatalf(`email_verified "true" = %+v, %v`, id, err)
	}
	iss.claims = map[string]any{"sub": "user-3", "email": "c@example.com"}
	if id, err := run("verifier-3", "nonce-3"); err != nil || id.EmailVerified {
		t.Fatalf("no email_verified = %+v, %v", id, err)
	}

	// A wrong PKCE verifier: the provider refuses the code.
	authURL, _ := p.AuthCodeURL(auth.AuthorizationRequest{State: "st", Nonce: "n", CodeChallenge: pkce("right"), CodeChallengeMethod: "S256"})
	code, _ := iss.authorize(t, authURL)
	if _, err := p.Exchange(ctx, auth.ExchangeRequest{Code: code, CodeVerifier: "wrong", Nonce: "n"}); err == nil {
		t.Fatal("a wrong verifier was accepted")
	}

	for name, tamper := range map[string]func(*jwt.Claims, *string){
		"nonce":    func(_ *jwt.Claims, n *string) { *n = "another" },
		"audience": func(c *jwt.Claims, _ *string) { c.Audience = jwt.Audience{"someone-else"} },
		"issuer":   func(c *jwt.Claims, _ *string) { c.Issuer = "https://evil.example.com" },
		"expired":  func(c *jwt.Claims, _ *string) { c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Hour)) },
	} {
		iss.tamper = tamper
		if _, err := run("v-"+name, "n-"+name); err == nil {
			t.Errorf("an ID token with another %s was accepted", name)
		}
	}
	iss.tamper = nil

	// Signed by a key the provider does not publish.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss.signer, _ = jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: other, KeyID: "k1"}}, nil)
	if _, err := run("v-key", "n-key"); err == nil {
		t.Error("an ID token signed by another key was accepted")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	iss := newIssuer(t)
	ctx := context.Background()
	full := oidc.Config{Name: "test", Issuer: iss.srv.URL, ClientID: clientID, RedirectURL: redirectURL}
	for name, mutate := range map[string]func(*oidc.Config){
		"name":     func(c *oidc.Config) { c.Name = "" },
		"issuer":   func(c *oidc.Config) { c.Issuer = "" },
		"client":   func(c *oidc.Config) { c.ClientID = "" },
		"redirect": func(c *oidc.Config) { c.RedirectURL = "" },
		"discovery": func(c *oidc.Config) {
			c.Issuer = iss.srv.URL + "/nowhere"
		},
	} {
		cfg := full
		mutate(&cfg)
		if _, err := oidc.New(ctx, cfg); err == nil {
			t.Errorf("%s: New accepted the config", name)
		}
	}
	p := iss.provider(t)
	if _, err := p.AuthCodeURL(auth.AuthorizationRequest{State: "s"}); err == nil {
		t.Error("AuthCodeURL accepted a request without a nonce and a challenge")
	}
}

// TestSignInThroughTheFramework: the adapter behind the framework's routes,
// from the sign-in's start to the session.
func TestSignInThroughTheFramework(t *testing.T) {
	gin.SetMode(gin.TestMode)
	iss := newIssuer(t)
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "oidc.db") + "?_fk=1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = strings.Repeat("j", 32)
	cfg.Auth.BcryptCost = bcrypt.MinCost
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()),
		framework.WithAuthIdentityProviders(iss.provider(t)))
	if err != nil {
		t.Fatal(err)
	}
	serve := func(req *http.Request, cookies []*http.Cookie) *httptest.ResponseRecorder {
		for _, c := range cookies {
			req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value}) //nolint:gosec // G124: request Cookie header only carries name/value.
		}
		rec := httptest.NewRecorder()
		app.Router().ServeHTTP(rec, req)
		return rec
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/external/test/login", strings.NewReader(`{"return_to":"/home"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(req, nil)
	var start struct {
		Data struct {
			URL string `json:"authorization_url"`
		} `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &start) != nil {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	code, state := iss.authorize(t, start.Data.URL)
	rec = serve(httptest.NewRequest(http.MethodGet, "/api/v1/auth/external/test/callback?"+url.Values{"code": {code}, "state": {state}}.Encode(), nil), rec.Result().Cookies())
	loc, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil || loc.Path != "/home" || loc.Query().Get("gombit_code") == "" {
		t.Fatalf("callback = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/external/exchange", strings.NewReader(`{"code":"`+loc.Query().Get("gombit_code")+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = serve(req, nil)
	var pair struct {
		Data struct {
			Access string `json:"access_token"`
		} `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &pair) != nil {
		t.Fatalf("exchange = %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+pair.Data.Access)
	if rec := serve(req, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"ada@example.com"`) {
		t.Fatalf("/me = %d %s", rec.Code, rec.Body.String())
	}
}
