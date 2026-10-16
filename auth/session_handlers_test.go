package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

type sessionView struct {
	ID              string `json:"id"`
	LastRefreshedAt string `json:"last_refreshed_at"`
	ExpiresAt       string `json:"expires_at"`
	Current         bool   `json:"current"`
}

func bearerRequest(app *framework.App, method, path, access string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	app.Router().ServeHTTP(rec, req)
	return rec
}

func decodeSessions(t *testing.T, rec *httptest.ResponseRecorder) []sessionView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var envelope dataBody[[]sessionView]
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode sessions: %v; body: %s", err, rec.Body.String())
	}
	return envelope.Data
}

// splitCurrent returns the current session's id and the other sessions' ids,
// failing t unless exactly one session is current.
func splitCurrent(t *testing.T, sessions []sessionView) (string, []string) {
	t.Helper()
	var current string
	var others []string
	for _, s := range sessions {
		if s.ID == "" || s.LastRefreshedAt == "" || s.ExpiresAt == "" {
			t.Fatalf("session missing fields: %+v", s)
		}
		if !s.Current {
			others = append(others, s.ID)
			continue
		}
		if current != "" {
			t.Fatalf("two sessions marked current: %+v", sessions)
		}
		current = s.ID
	}
	if current == "" {
		t.Fatalf("no session marked current: %+v", sessions)
	}
	return current, others
}

func TestSessionsBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newAuthApp(t)
	runBearerSessions(t, app)
}

// runBearerSessions exercises the sessions API end to end in Bearer mode; the
// integration tests run it against PostgreSQL and MySQL.
func runBearerSessions(t *testing.T, app *framework.App) {
	t.Helper()
	const sessions = "/api/v1/auth/sessions"
	registerUser(t, app, "sessions@example.com", testPassword)
	laptop := loginUser(t, app, "sessions@example.com", testPassword)
	phone := loginUser(t, app, "sessions@example.com", testPassword)
	registerUser(t, app, "bystander@example.com", testPassword)
	bystander := loginUser(t, app, "bystander@example.com", testPassword)

	listed := decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, laptop.AccessToken))
	if len(listed) != 2 {
		t.Fatalf("listed %d sessions, want 2 (another user's session must not appear): %+v", len(listed), listed)
	}
	laptopID, others := splitCurrent(t, listed)
	phoneID := others[0]
	if again, _ := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, phone.AccessToken))); again != phoneID {
		t.Fatalf("phone sees itself as %q, laptop listed it as %q", again, phoneID)
	}

	t.Run("unauthenticated", func(t *testing.T) {
		assertError(t, bearerRequest(app, http.MethodGet, sessions, ""), http.StatusUnauthorized, "authentication")
		assertError(t, bearerRequest(app, http.MethodDelete, sessions+"?scope=all", ""), http.StatusUnauthorized, "authentication")
	})

	t.Run("scope is required and closed", func(t *testing.T) {
		for _, path := range []string{sessions, sessions + "?scope=everyone"} {
			assertError(t, bearerRequest(app, http.MethodDelete, path, laptop.AccessToken), http.StatusUnprocessableEntity, "validation_error")
		}
	})

	t.Run("unknown and foreign ids are not found", func(t *testing.T) {
		bystanderID, _ := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, bystander.AccessToken)))
		for _, id := range []string{"00000000000000000000000000000000", bystanderID} {
			assertError(t, bearerRequest(app, http.MethodDelete, sessions+"/"+id, laptop.AccessToken), http.StatusNotFound, "not_found")
		}
		if status := getMe(t, app, bystander.AccessToken); status != http.StatusOK {
			t.Fatalf("bystander GET /me = %d, want 200", status)
		}
	})

	t.Run("revoke one", func(t *testing.T) {
		rec := bearerRequest(app, http.MethodDelete, sessions+"/"+phoneID, laptop.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, phone.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("revoked phone GET /me = %d, want 401", status)
		}
		// The revoked device refreshing is a dead credential, not reuse: it
		// must not end the laptop's session.
		assertError(t, postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+phone.RefreshToken+`"}`), http.StatusUnauthorized, "authentication")
		if status := getMe(t, app, laptop.AccessToken); status != http.StatusOK {
			t.Fatalf("laptop GET /me after the phone's refresh attempt = %d, want 200", status)
		}
		assertError(t, bearerRequest(app, http.MethodDelete, sessions+"/"+phoneID, laptop.AccessToken), http.StatusNotFound, "not_found")
	})

	t.Run("revoke others", func(t *testing.T) {
		tablet := loginUser(t, app, "sessions@example.com", testPassword)
		rec := bearerRequest(app, http.MethodDelete, sessions+"?scope=others", laptop.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=others status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, tablet.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("tablet GET /me = %d, want 401", status)
		}
		remaining := decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, laptop.AccessToken))
		if current, others := splitCurrent(t, remaining); current != laptopID || len(others) != 0 {
			t.Fatalf("after scope=others: current %q others %v, want only %q", current, others, laptopID)
		}
	})

	t.Run("revoke the current session", func(t *testing.T) {
		own := loginUser(t, app, "sessions@example.com", testPassword)
		ownID, _ := splitCurrent(t, decodeSessions(t, bearerRequest(app, http.MethodGet, sessions, own.AccessToken)))
		rec := bearerRequest(app, http.MethodDelete, sessions+"/"+ownID, own.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE own session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, own.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("GET /me after revoking the session = %d, want 401", status)
		}
		assertError(t, postJSON(t, app, "/api/v1/auth/refresh", `{"refresh_token":"`+own.RefreshToken+`"}`), http.StatusUnauthorized, "authentication")
		if status := getMe(t, app, laptop.AccessToken); status != http.StatusOK {
			t.Fatalf("laptop GET /me = %d, want 200: revoking one's own session must not end the others", status)
		}
	})

	t.Run("revoke all", func(t *testing.T) {
		rec := bearerRequest(app, http.MethodDelete, sessions+"?scope=all", laptop.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=all status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if status := getMe(t, app, laptop.AccessToken); status != http.StatusUnauthorized {
			t.Fatalf("laptop GET /me after scope=all = %d, want 401", status)
		}
		if status := getMe(t, app, bystander.AccessToken); status != http.StatusOK {
			t.Fatalf("bystander GET /me = %d, want 200", status)
		}
	})
}

func TestSessionsCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runCookieSessions(t, newCookieAuthApp(t))
}

// runCookieSessions exercises the sessions API end to end in cookie mode; the
// integration tests run it against PostgreSQL and MySQL.
func runCookieSessions(t *testing.T, app *framework.App) {
	t.Helper()
	const sessions = "/api/v1/auth/sessions"

	login := func(t *testing.T) *cookieJar {
		t.Helper()
		jar := fetchCSRF(t, app)
		if rec := loginCookieUser(t, app, jar, "cookie-sessions@example.com", testPassword); rec.Code != http.StatusOK {
			t.Fatalf("login status = %d; body: %s", rec.Code, rec.Body.String())
		}
		return jar
	}
	registerCookieUser(t, app, fetchCSRF(t, app), "cookie-sessions@example.com", testPassword)
	laptop := login(t)
	phone := login(t)

	laptopID, others := splitCurrent(t, decodeSessions(t, doRequest(app, laptop, http.MethodGet, sessions, "")))
	if len(others) != 1 {
		t.Fatalf("laptop sees %d other sessions, want 1", len(others))
	}

	t.Run("state-changing calls need the CSRF header", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, sessions+"?scope=others", nil)
		laptop.attach(req)
		app.Router().ServeHTTP(rec, req)
		assertCSRFRejected(t, rec)
		if status := getMeCookie(app, phone); status != http.StatusOK {
			t.Fatalf("phone GET /me after a CSRF-rejected revoke = %d, want 200", status)
		}
	})

	t.Run("revoking another session keeps the current cookies", func(t *testing.T) {
		tablet := login(t)
		tabletID, _ := splitCurrent(t, decodeSessions(t, doRequest(app, tablet, http.MethodGet, sessions, "")))
		rec := doRequest(app, laptop, http.MethodDelete, sessions+"/"+tabletID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE another session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Cookies(); len(got) != 0 {
			t.Fatalf("revoking another session set cookies %v, want none", got)
		}
		if status := getMeCookie(app, tablet); status != http.StatusUnauthorized {
			t.Fatalf("tablet GET /me = %d, want 401", status)
		}
		if status := getMeCookie(app, laptop); status != http.StatusOK {
			t.Fatalf("laptop GET /me = %d, want 200: it revoked another session, not its own", status)
		}
	})

	t.Run("revoke others keeps the current cookies", func(t *testing.T) {
		rec := doRequest(app, laptop, http.MethodDelete, sessions+"?scope=others", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=others status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Result().Cookies(); len(got) != 0 {
			t.Fatalf("scope=others set cookies %v, want none", got)
		}
		if status := getMeCookie(app, phone); status != http.StatusUnauthorized {
			t.Fatalf("phone GET /me = %d, want 401", status)
		}
		if status := getMeCookie(app, laptop); status != http.StatusOK {
			t.Fatalf("laptop GET /me = %d, want 200", status)
		}
	})

	t.Run("revoking the current session clears its cookies", func(t *testing.T) {
		rec := doRequest(app, laptop, http.MethodDelete, sessions+"/"+laptopID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE own session status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if laptop.value(auth.AccessCookieName) != "" || laptop.value(auth.RefreshCookieName) != "" {
			t.Fatalf("session cookies not cleared: %+v", laptop.cookies)
		}
		if status := getMeCookie(app, laptop); status != http.StatusUnauthorized {
			t.Fatalf("laptop GET /me = %d, want 401", status)
		}
	})

	t.Run("revoke all clears the cookies", func(t *testing.T) {
		tablet := login(t)
		rec := doRequest(app, tablet, http.MethodDelete, sessions+"?scope=all", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE scope=all status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if tablet.value(auth.AccessCookieName) != "" || tablet.value(auth.RefreshCookieName) != "" {
			t.Fatalf("scope=all did not clear both session cookies: %+v", tablet.cookies)
		}
		cleared := map[string]bool{}
		for _, c := range rec.Result().Cookies() {
			if c.MaxAge < 0 && c.Value == "" {
				cleared[c.Name] = true
			}
		}
		if !cleared[auth.AccessCookieName] || !cleared[auth.RefreshCookieName] {
			t.Fatalf("scope=all Set-Cookie headers %v, want %s and %s both expired", rec.Result().Cookies(), auth.AccessCookieName, auth.RefreshCookieName)
		}
		if status := getMeCookie(app, tablet); status != http.StatusUnauthorized {
			t.Fatalf("tablet GET /me = %d, want 401", status)
		}
	})
}

// TestSessionExpiryGates runs runSessionExpiryGates on SQLite.
func TestSessionExpiryGates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runSessionExpiryGates(t, openSQLite(t))
}

// runSessionExpiryGates checks the gates every authenticated route sits
// behind, the Bearer one and the cookie one (RequireCookieSession, which the
// admin mounts too): an access token that is still live does not outlive its
// session. The refresh TTL is at least the access TTL, so only a token issued
// before that rule, or one whose session row's expiry changed, can; the
// test shortens the row to 30 seconds and moves on 31, with the 1m access
// token still live: the gate answers 401 and the route's handler does not
// run, as ParseAccess refuses the token. The integration tests run it
// against PostgreSQL and MySQL.
func runSessionExpiryGates(t *testing.T, db *database.DB) {
	t.Helper()
	tests := []struct {
		name      string
		app       func(*testing.T, *database.DB) *framework.App
		gate      func(*auth.Service) func(huma.Context, func(huma.Context))
		send      func(req *http.Request, access string)
		challenge string
	}{
		{
			name: "bearer",
			app:  newAuthAppWithDB,
			gate: auth.RequireBearer,
			send: func(req *http.Request, access string) {
				req.Header.Set("Authorization", "Bearer "+access)
			},
			challenge: `Bearer realm="api"`,
		},
		{
			name: "cookie",
			app:  newCookieAuthAppWithDB,
			gate: (*auth.Service).RequireCookieSession,
			send: func(req *http.Request, access string) {
				req.AddCookie(&http.Cookie{Name: auth.AccessCookieName, Value: access}) //nolint:gosec // G124: request Cookie header only carries name/value.
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := tt.app(t, db)
			e := newSessionsEnv(t, db, fmt.Sprintf("gate-%s-%d", tt.name, time.Now().UnixNano()), time.Minute, time.Hour)
			clock := &stepClock{now: time.Now()}
			auth.SetClock(e.svc, clock)
			_, pairs := e.user(t, "gate", 1)
			ran := false
			path := "/session-expiry-gate/" + tt.name
			huma.Register(app.API(), huma.Operation{
				OperationID: "session-expiry-gate-" + tt.name,
				Method:      http.MethodGet,
				Path:        path,
				Middlewares: huma.Middlewares{tt.gate(e.svc)},
			}, func(context.Context, *struct{}) (*struct{}, error) {
				ran = true
				return nil, nil
			})
			request := func() *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				tt.send(req, pairs[0].AccessToken)
				app.Router().ServeHTTP(rec, req)
				return rec
			}

			if rec := request(); rec.Code >= http.StatusMultipleChoices || !ran {
				t.Fatalf("active session: status %d, handler ran %v; want it through; body: %s", rec.Code, ran, rec.Body.String())
			}
			ran = false
			row, err := auth.AuthenticatedRow(e.ctx, e.svc, pairs[0].AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&auth.RefreshToken{}).Where("id = ?", row).
				Update("expires_at", clock.now.Add(30*time.Second)).Error; err != nil {
				t.Fatal(err)
			}
			clock.now = clock.now.Add(31 * time.Second) // the session expires; the access token does not
			rec := request()
			assertError(t, rec, http.StatusUnauthorized, "authentication")
			if ran {
				t.Fatal("the handler ran for an access token whose session has expired")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != tt.challenge {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, tt.challenge)
			}
		})
	}
}

// TestIssuedAccessLifetime: login and refresh describe one access-token
// lifetime, the access TTL, in everything they issue (expires_in, the JWT's
// exp, the access cookie's Max-Age); the token's session lasts at least as
// long; and the gate accepts the token up to that exp and refuses it after.
func TestIssuedAccessLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runIssuedAccessLifetime(t, openSQLite(t))
}

// issuedAccess is what a login or a refresh issued: the access token and the
// lifetimes the response gave it (expires_in in Bearer mode, the session
// cookies' Max-Age in cookie mode; zero where the mode has none).
type issuedAccess struct {
	token         string
	expiresIn     int
	accessMaxAge  int
	refreshMaxAge int
}

// runIssuedAccessLifetime logs in and refreshes over HTTP in both auth modes,
// with an access TTL below the refresh TTL and equal to it, and checks what
// each issued against the gate. The app's own Service runs on the system
// clock and is not reachable from a test, so the gate is a second Service
// over the same database and config, on a clock the test sets, mounted on a
// route of the app, as in runSessionExpiryGates: it validates the issued
// token as the app's gate does. The integration tests run it against
// PostgreSQL and MySQL.
func runIssuedAccessLifetime(t *testing.T, db *database.DB) {
	t.Helper()
	modes := []struct {
		name  string
		mode  config.AuthMode
		gate  func(*auth.Service) func(huma.Context, func(huma.Context))
		send  func(req *http.Request, access string)
		login func(t *testing.T, app *framework.App, email string) (issuedAccess, func(*testing.T) issuedAccess)
	}{
		{
			name: "bearer",
			mode: config.AuthModeJWT,
			gate: auth.RequireBearer,
			send: func(req *http.Request, access string) {
				req.Header.Set("Authorization", "Bearer "+access)
			},
			login: func(t *testing.T, app *framework.App, email string) (issuedAccess, func(*testing.T) issuedAccess) {
				registerUser(t, app, email, testPassword)
				pair := loginUser(t, app, email, testPassword)
				refresh := func(t *testing.T) issuedAccess {
					next := refreshUser(t, app, pair.RefreshToken)
					return issuedAccess{token: next.AccessToken, expiresIn: next.ExpiresIn}
				}
				return issuedAccess{token: pair.AccessToken, expiresIn: pair.ExpiresIn}, refresh
			},
		},
		{
			name: "cookie",
			mode: config.AuthModeCookie,
			gate: (*auth.Service).RequireCookieSession,
			send: func(req *http.Request, access string) {
				req.AddCookie(&http.Cookie{Name: auth.AccessCookieName, Value: access}) //nolint:gosec // G124: request Cookie header only carries name/value.
			},
			login: func(t *testing.T, app *framework.App, email string) (issuedAccess, func(*testing.T) issuedAccess) {
				jar := fetchCSRF(t, app)
				registerCookieUser(t, app, jar, email, testPassword)
				login := sessionCookiesOf(t, loginCookieUser(t, app, jar, email, testPassword))
				refresh := func(t *testing.T) issuedAccess {
					return sessionCookiesOf(t, doRequest(app, jar, http.MethodPost, "/api/v1/auth/refresh", ""))
				}
				return login, refresh
			},
		},
	}
	ttls := []struct{ access, refresh time.Duration }{
		{access: time.Minute, refresh: time.Hour},
		{access: 10 * time.Minute, refresh: 10 * time.Minute},
	}
	for _, mode := range modes {
		for _, ttl := range ttls {
			t.Run(fmt.Sprintf("%s access %s refresh %s", mode.name, ttl.access, ttl.refresh), func(t *testing.T) {
				app := newAuthAppWithTTLs(t, db, mode.mode, ttl.access, ttl.refresh)
				tag := fmt.Sprintf("lifetime-%s-%d", mode.name, time.Now().UnixNano())
				gate := newSessionsEnv(t, db, tag, ttl.access, ttl.refresh)
				clock := &stepClock{}
				auth.SetClock(gate.svc, clock)
				ran := false
				path := "/issued-lifetime-gate"
				huma.Register(app.API(), huma.Operation{
					OperationID: "issued-lifetime-gate",
					Method:      http.MethodGet,
					Path:        path,
					Middlewares: huma.Middlewares{mode.gate(gate.svc)},
				}, func(context.Context, *struct{}) (*struct{}, error) {
					ran = true
					return nil, nil
				})
				request := func(access string, at time.Time) *httptest.ResponseRecorder {
					clock.now = at
					ran = false
					rec := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodGet, path, nil)
					mode.send(req, access)
					app.Router().ServeHTTP(rec, req)
					return rec
				}
				check := func(t *testing.T, step string, got issuedAccess) {
					t.Helper()
					claims := accessClaimsOf(t, got.token)
					accessSeconds, refreshSeconds := int(ttl.access/time.Second), int(ttl.refresh/time.Second)
					if lifetime := claims.Exp - claims.Iat; lifetime != int64(accessSeconds) {
						t.Fatalf("%s: the JWT's exp - iat = %ds, want the access TTL, %ds", step, lifetime, accessSeconds)
					}
					if mode.mode == config.AuthModeCookie {
						if got.accessMaxAge != accessSeconds || got.refreshMaxAge != refreshSeconds {
							t.Fatalf("%s: cookie Max-Age access %d, refresh %d; want the TTLs, %d and %d",
								step, got.accessMaxAge, got.refreshMaxAge, accessSeconds, refreshSeconds)
						}
					} else if got.expiresIn != accessSeconds {
						t.Fatalf("%s: expires_in = %d, want the access TTL, %d", step, got.expiresIn, accessSeconds)
					}
					exp := time.Unix(claims.Exp, 0)
					var row auth.RefreshToken
					if err := db.First(&row, claims.Rid).Error; err != nil {
						t.Fatalf("%s: load the token's session row %d: %v", step, claims.Rid, err)
					}
					if exp.After(row.ExpiresAt) {
						t.Fatalf("%s: the access token expires at %v, after its session (%v)", step, exp, row.ExpiresAt)
					}
					if rec := request(got.token, exp.Add(-time.Second)); rec.Code >= http.StatusMultipleChoices || !ran {
						t.Fatalf("%s: a second before exp: status %d, handler ran %v; want it through; body: %s",
							step, rec.Code, ran, rec.Body.String())
					}
					rec := request(got.token, exp.Add(time.Second))
					assertError(t, rec, http.StatusUnauthorized, "authentication")
					if ran {
						t.Fatalf("%s: the handler ran a second after the access token's exp", step)
					}
				}

				login, refresh := mode.login(t, app, tag+"@example.com")
				check(t, "login", login)
				check(t, "refresh", refresh(t))
			})
		}
	}
}

// sessionCookiesOf reads the session cookies a cookie-mode login or refresh
// set.
func sessionCookiesOf(t *testing.T, rec *httptest.ResponseRecorder) issuedAccess {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var got issuedAccess
	var access, refresh bool
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case auth.AccessCookieName:
			got.token, got.accessMaxAge, access = c.Value, c.MaxAge, true
		case auth.RefreshCookieName:
			got.refreshMaxAge, refresh = c.MaxAge, true
		}
	}
	if !access || !refresh {
		t.Fatalf("the response did not set both session cookies; headers: %v", rec.Header())
	}
	return got
}

// accessClaims is the part of an access JWT's payload the lifetime checks
// read.
type accessClaims struct {
	Iat int64 `json:"iat"`
	Exp int64 `json:"exp"`
	Rid uint  `json:"rid"`
}

// accessClaimsOf decodes an access JWT's payload; the gate verifies the
// signature.
func accessClaimsOf(t *testing.T, token string) accessClaims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("access token %q is not a JWT", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode the JWT payload: %v", err)
	}
	var claims accessClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode the JWT payload %s: %v", payload, err)
	}
	if claims.Iat == 0 || claims.Exp == 0 || claims.Rid == 0 {
		t.Fatalf("the JWT payload %s lacks iat, exp or rid", payload)
	}
	return claims
}

// TestSessionsOpenAPI: in each auth mode the session operations carry their
// operation IDs and that mode's security scheme (bearerAuth or cookieAuth,
// never both), and DELETE /auth/sessions requires a scope of exactly others
// or all.
func TestSessionsOpenAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	modes := []struct {
		name   string
		app    func(*testing.T) *framework.App
		scheme string
	}{
		{"bearer", newAuthApp, "bearerAuth"},
		{"cookie", newCookieAuthApp, "cookieAuth"},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mode.app(t).Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /openapi.json status = %d; body: %s", rec.Code, rec.Body.String())
			}
			var spec struct {
				Paths map[string]map[string]struct {
					OperationID string                `json:"operationId"`
					Security    []map[string][]string `json:"security"`
					Parameters  []struct {
						Name     string `json:"name"`
						In       string `json:"in"`
						Required bool   `json:"required"`
						Schema   struct {
							Enum []string `json:"enum"`
						} `json:"schema"`
					} `json:"parameters"`
				} `json:"paths"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
				t.Fatalf("decode OpenAPI: %v", err)
			}
			for _, op := range []struct{ path, method, id string }{
				{"/api/v1/auth/sessions", "get", "auth-list-sessions"},
				{"/api/v1/auth/sessions/{id}", "delete", "auth-revoke-session"},
				{"/api/v1/auth/sessions", "delete", "auth-revoke-sessions"},
			} {
				got, ok := spec.Paths[op.path][op.method]
				if !ok {
					t.Fatalf("OpenAPI has no %s %s", op.method, op.path)
				}
				if got.OperationID != op.id {
					t.Errorf("%s %s operationId = %q, want %q", op.method, op.path, got.OperationID, op.id)
				}
				if len(got.Security) != 1 {
					t.Fatalf("%s security = %v, want exactly %s", op.id, got.Security, mode.scheme)
				}
				if _, ok := got.Security[0][mode.scheme]; !ok || len(got.Security[0]) != 1 {
					t.Errorf("%s security = %v, want only %s", op.id, got.Security, mode.scheme)
				}
			}
			revoke := spec.Paths["/api/v1/auth/sessions"]["delete"]
			for _, p := range revoke.Parameters {
				if p.Name != "scope" {
					continue
				}
				if p.In != "query" || !p.Required {
					t.Errorf("scope parameter in=%q required=%v, want a required query parameter", p.In, p.Required)
				}
				if len(p.Schema.Enum) != 2 || p.Schema.Enum[0] != "others" || p.Schema.Enum[1] != "all" {
					t.Errorf("scope enum = %v, want [others all]", p.Schema.Enum)
				}
				return
			}
			t.Fatalf("auth-revoke-sessions has no scope parameter: %+v", revoke.Parameters)
		})
	}
}

// TestSessionsLeaveResourceOperationIDsFree: an app that scaffolds a resource
// named Session (the example `gombit make resource --help` gives) registers
// list-sessions, get-session and create-session on the API auth mounted on,
// since resourcegen/modelhandler.go names a resource's operations
// "list-"+kebab, "get-"+package and "create-"+package. Huma panics on a
// duplicate operation ID, so in either auth mode such an app would not boot if
// the session routes took one of those names.
func TestSessionsLeaveResourceOperationIDsFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []struct {
		name string
		app  func(*testing.T) *framework.App
	}{
		{"bearer", newAuthApp},
		{"cookie", newCookieAuthApp},
	} {
		t.Run(mode.name, func(t *testing.T) {
			api := mode.app(t).API()
			for _, op := range []struct{ id, method, path string }{
				{"list-sessions", http.MethodGet, "/api/v1/sessions"},
				{"get-session", http.MethodGet, "/api/v1/sessions/{id}"},
				{"create-session", http.MethodPost, "/api/v1/sessions"},
			} {
				registerResourceOperation(t, api, op.id, op.method, op.path)
			}
		})
	}
}

// registerResourceOperation registers an operation the way a generated
// resource's Register does, failing t instead of panicking.
func registerResourceOperation(t *testing.T, api huma.API, id, method, path string) {
	t.Helper()
	mustRegister(t, "a generated resource's "+id, func() {
		huma.Register(api, huma.Operation{
			OperationID: id,
			Method:      method,
			Path:        path,
			Tags:        []string{"Sessions"},
		}, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	})
}

// Session and RevokeResult stand for an application's own types whose names
// the sessions API's schemas could have taken.
type Session struct {
	Topic string `json:"topic"`
}

type RevokeResult struct {
	Done bool `json:"done"`
}

type appSessionsOutput struct {
	Body contract.Data[[]Session]
}

type appRevokeOutput struct {
	Body contract.Data[RevokeResult]
}

// TestSessionsLeaveAppSchemaNamesFree: Huma names a schema after its Go type,
// without the package, in one registry every operation of the app shares, and
// panics when two different types get the same name. An application with its
// own Session type (a workout session, a chat session) must still boot next
// to the sessions API in either auth mode, also inside the D10 envelope,
// where contract.Data[[]Session] is named DataListSession.
func TestSessionsLeaveAppSchemaNamesFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []struct {
		name string
		app  func(*testing.T) *framework.App
	}{
		{"bearer", newAuthApp},
		{"cookie", newCookieAuthApp},
	} {
		t.Run(mode.name, func(t *testing.T) {
			api := mode.app(t).API()
			mustRegister(t, "an app operation returning its own Session list", func() {
				huma.Register(api, huma.Operation{
					OperationID: "list-workout-sessions",
					Method:      http.MethodGet,
					Path:        "/api/v1/workout-sessions",
				}, func(context.Context, *struct{}) (*appSessionsOutput, error) { return nil, nil })
			})
			mustRegister(t, "an app operation returning its own RevokeResult", func() {
				huma.Register(api, huma.Operation{
					OperationID: "revoke-workout-sessions",
					Method:      http.MethodDelete,
					Path:        "/api/v1/workout-sessions",
				}, func(context.Context, *struct{}) (*appRevokeOutput, error) { return nil, nil })
			})
		})
	}
}

// mustRegister runs register, failing t instead of panicking: Huma panics
// when an operation repeats an operation ID or a schema name.
func mustRegister(t *testing.T, what string, register func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering %s after auth.Mount: %v", what, r)
		}
	}()
	register()
}
