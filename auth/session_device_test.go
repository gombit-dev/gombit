package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

// newDeviceEnv is a sessions env whose Service keeps client addresses as
// mode says, with opts.
func newDeviceEnv(t *testing.T, db *database.DB, mode config.SessionClientAddress, opts ...auth.Option) sessionsEnv {
	t.Helper()
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	cfg.Auth.SessionClientAddress = mode
	svc, err := auth.NewService(db.DB, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return sessionsEnv{svc: svc, ctx: context.Background(), tag: fmt.Sprintf("device-%d", time.Now().UnixNano())}
}

// signInFrom registers a user unique to e and starts a session for it from
// a request with the client address ip and the User-Agent ua.
func signInFrom(t *testing.T, e sessionsEnv, name, ip, ua string) (auth.User, auth.TokenPair) {
	t.Helper()
	user, _ := e.user(t, name, 0)
	pair, err := e.svc.IssueTokens(auth.ContextWithRequestInfo(e.ctx, ip, ua), user)
	if err != nil {
		t.Fatalf("IssueTokens: %v", err)
	}
	return user, pair
}

// onlySession lists the user's sessions, failing t unless there is one.
func onlySession(t *testing.T, e sessionsEnv, userID uint) auth.AuthSession {
	t.Helper()
	sessions, err := e.svc.ListSessions(e.ctx, userID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListSessions = %+v, %v; want one session", sessions, err)
	}
	return sessions[0]
}

// runSessionDevice runs the AUTH-12 scenarios, what a session keeps about
// the device that started it, against db; SQLite, PostgreSQL and MySQL all
// run them.
func runSessionDevice(t *testing.T, db *database.DB) {
	t.Helper()
	const firefox = "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0"

	// The sign-in's device is the session's: a refresh from another
	// address and browser changes none of it.
	t.Run("recorded at sign-in, kept by refreshes", func(t *testing.T) {
		e := newDeviceEnv(t, db, config.SessionClientAddressCoarse)
		ada, pair := signInFrom(t, e, "ada", "203.0.113.7", firefox)
		elsewhere := auth.ContextWithRequestInfo(e.ctx, "198.51.100.9", "curl/8.5.0")
		for range 2 {
			next, err := e.svc.RotateRefresh(elsewhere, pair.RefreshToken)
			if err != nil {
				t.Fatalf("RotateRefresh: %v", err)
			}
			pair = next
		}
		got := onlySession(t, e, ada.ID)
		if got.UserAgent != firefox || got.ClientAddress != "203.0.113.0/24" || got.Location != "" {
			t.Fatalf("after two refreshes elsewhere the session is %+v; want the sign-in's user agent and network, no location", got)
		}
	})

	// How much of the address a session keeps: by default its network,
	// otherwise all or none of it. An IPv4 address mapped into IPv6 is the
	// IPv4 address, a zone is no part of an address, and what does not
	// parse as an address is none.
	t.Run("client address", func(t *testing.T) {
		tests := []struct {
			ip   string
			mode config.SessionClientAddress
			want string
		}{
			{"203.0.113.7", config.SessionClientAddressCoarse, "203.0.113.0/24"},
			{"203.0.113.7", "", "203.0.113.0/24"},
			{"203.0.113.7", config.SessionClientAddressNone, ""},
			{"203.0.113.7", config.SessionClientAddressFull, "203.0.113.7"},
			{"2001:db8:1234:5678::1", config.SessionClientAddressCoarse, "2001:db8:1234::/48"},
			{"2001:db8:1234:5678::1", config.SessionClientAddressFull, "2001:db8:1234:5678::1"},
			{"::ffff:203.0.113.7", config.SessionClientAddressCoarse, "203.0.113.0/24"},
			{"::ffff:203.0.113.7", config.SessionClientAddressFull, "203.0.113.7"},
			{"fe80::1%eth0", config.SessionClientAddressFull, "fe80::1"},
			{"not-an-address", config.SessionClientAddressFull, ""},
			{"", config.SessionClientAddressCoarse, ""},
		}
		for i, tt := range tests {
			t.Run(fmt.Sprintf("%s %s", tt.ip, tt.mode), func(t *testing.T) {
				e := newDeviceEnv(t, db, tt.mode)
				ada, _ := signInFrom(t, e, fmt.Sprintf("addr%d", i), tt.ip, firefox)
				if got := onlySession(t, e, ada.ID).ClientAddress; got != tt.want {
					t.Fatalf("client address = %q, want %q", got, tt.want)
				}
			})
		}
	})

	// The User-Agent is kept as given, as far as one line of text in a
	// column of every supported database can keep it: invalid UTF-8
	// replaced, control characters of every range (a NUL, which PostgreSQL
	// refuses; a C1 NEL), line separators and bidi overrides dropped, cut at
	// a character boundary to 255 bytes. The sign-in succeeds either way.
	t.Run("user agent the database can store", func(t *testing.T) {
		e := newDeviceEnv(t, db, config.SessionClientAddressCoarse)
		ua := "a\x00b\xffc\t\u0085\u2028\u202e" + strings.Repeat("é", 200)
		ada, _ := signInFrom(t, e, "ua", "203.0.113.7", ua)
		got := onlySession(t, e, ada.ID).UserAgent
		if !strings.HasPrefix(got, "ab�c") || len(got) > 255 || len(got) < 250 || !utf8.ValidString(got) || strings.ContainsAny(got, "\x00\t\u0085\u2028\u202e") {
			t.Fatalf("user agent = %q (%d bytes); want the given one without control characters, invalid bytes replaced, cut to 255 bytes on a character", got, len(got))
		}
	})

	// A SessionLocator names the location from the full address, whatever
	// the address mode stores, once per sign-in: a refresh keeps its answer
	// (cut to 128 bytes) and does not ask again.
	t.Run("locator", func(t *testing.T) {
		var asked []netip.Addr
		locate := func(_ context.Context, addr netip.Addr) string {
			asked = append(asked, addr)
			return "Lisbon, PT " + strings.Repeat("x", 200)
		}
		e := newDeviceEnv(t, db, config.SessionClientAddressNone, auth.WithSessionLocator(locate))
		ada, pair := signInFrom(t, e, "located", "203.0.113.7", firefox)
		e.rotate(t, pair)
		if len(asked) != 1 || asked[0] != netip.MustParseAddr("203.0.113.7") {
			t.Fatalf("the locator was asked %v; want once, for 203.0.113.7", asked)
		}
		got := onlySession(t, e, ada.ID)
		if !strings.HasPrefix(got.Location, "Lisbon, PT ") || len(got.Location) != 128 || got.ClientAddress != "" {
			t.Fatalf("session = %+v; want the locator's answer cut to 128 bytes, and no address", got)
		}
	})

	// A session started outside a request (IssueTokens from application
	// code) knows nothing of a device, and neither do rows from before the
	// columns existed; such a session still lists and refreshes.
	t.Run("unknown device", func(t *testing.T) {
		e := newDeviceEnv(t, db, config.SessionClientAddressFull)
		ada, _ := e.user(t, "nodevice", 1)
		if got := onlySession(t, e, ada.ID); got.UserAgent != "" || got.ClientAddress != "" || got.Location != "" {
			t.Fatalf("a session started outside a request = %+v; want no device", got)
		}
		grace, pair := signInFrom(t, e, "preupgrade", "203.0.113.7", firefox)
		if err := db.Model(&auth.RefreshToken{}).Where("user_id = ?", grace.ID).Updates(map[string]any{
			"user_agent": gorm.Expr("NULL"), "client_address": gorm.Expr("NULL"), "location": gorm.Expr("NULL"),
		}).Error; err != nil {
			t.Fatal(err)
		}
		e.rotate(t, pair)
		if got := onlySession(t, e, grace.ID); got.UserAgent != "" || got.ClientAddress != "" || got.Location != "" {
			t.Fatalf("a session from before the device columns = %+v; want no device", got)
		}
	})
}

func TestSessionDeviceSQLite(t *testing.T) {
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	runSessionDevice(t, db)
}

// sendFrom is doRequest (jar may be nil) from a client at remote with the
// User-Agent ua, with the Bearer access token when access is set.
func sendFrom(app *framework.App, jar *cookieJar, method, path, body, access, remote, ua string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = remote
	req.Header.Set("User-Agent", ua)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	if jar != nil {
		jar.attach(req)
		if csrf := jar.value(auth.CSRFCookieName); csrf != "" {
			req.Header.Set(auth.CSRFHeaderName, csrf)
		}
	}
	app.Router().ServeHTTP(rec, req)
	if jar != nil {
		jar.update(rec)
	}
	return rec
}

// deviceView is the device GET /auth/sessions shows for a session.
type deviceView struct {
	UserAgent     string `json:"user_agent"`
	ClientAddress string `json:"client_address"`
	Location      string `json:"location"`
}

// currentDevice is the device GET /auth/sessions, answered in rec, shows
// for the session that asked, failing t unless exactly one is current.
func currentDevice(t *testing.T, rec *httptest.ResponseRecorder) deviceView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions = %d: %s", rec.Code, rec.Body.String())
	}
	var envelope dataBody[[]struct {
		deviceView
		Current bool `json:"current"`
	}]
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var found []deviceView
	for _, s := range envelope.Data {
		if s.Current {
			found = append(found, s.deviceView)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d current sessions in %s, want 1", len(found), rec.Body.String())
	}
	return found[0]
}

// TestSessionDeviceHTTP: in both auth modes GET /auth/sessions shows the
// device the session signed in from, its User-Agent and its address's
// network, also after a refresh from another device.
func TestSessionDeviceHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		laptop   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 Version/17.5 Safari/605.1.15"
		home     = "203.0.113.7:51000"
		other    = "curl/8.5.0"
		away     = "[2001:db8::5]:443"
		sessions = "/api/v1/auth/sessions"
	)
	type device = deviceView
	list := func(t *testing.T, rec *httptest.ResponseRecorder) []device {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /auth/sessions = %d: %s", rec.Code, rec.Body.String())
		}
		var envelope dataBody[[]device]
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	want := device{UserAgent: laptop, ClientAddress: "203.0.113.0/24"}

	t.Run("bearer", func(t *testing.T) {
		app := newAuthApp(t)
		registerUser(t, app, "device@example.com", testPassword)
		rec := sendFrom(app, nil, http.MethodPost, "/api/v1/auth/login", `{"email":"device@example.com","password":"`+testPassword+`"}`, "", home, laptop)
		if rec.Code != http.StatusOK {
			t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
		}
		var login dataBody[tokenPair]
		if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
			t.Fatal(err)
		}
		rec = sendFrom(app, nil, http.MethodPost, "/api/v1/auth/refresh", `{"refresh_token":"`+login.Data.RefreshToken+`"}`, "", away, other)
		var refreshed dataBody[tokenPair]
		if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("refresh = %d, %v: %s", rec.Code, err, rec.Body.String())
		}
		got := list(t, sendFrom(app, nil, http.MethodGet, sessions, "", refreshed.Data.AccessToken, away, other))
		if len(got) != 1 || got[0] != want {
			t.Fatalf("sessions = %+v, want [%+v]", got, want)
		}
	})

	t.Run("cookie", func(t *testing.T) {
		app := newCookieAuthApp(t)
		registerCookieUser(t, app, fetchCSRF(t, app), "device-cookie@example.com", testPassword)
		jar := fetchCSRF(t, app)
		if rec := sendFrom(app, jar, http.MethodPost, "/api/v1/auth/login", `{"email":"device-cookie@example.com","password":"`+testPassword+`"}`, "", home, laptop); rec.Code != http.StatusOK {
			t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
		}
		if rec := sendFrom(app, jar, http.MethodPost, "/api/v1/auth/refresh", "", "", away, other); rec.Code != http.StatusOK {
			t.Fatalf("refresh = %d: %s", rec.Code, rec.Body.String())
		}
		got := list(t, sendFrom(app, jar, http.MethodGet, sessions, "", "", away, other))
		if len(got) != 1 || got[0] != want {
			t.Fatalf("sessions = %+v, want [%+v]", got, want)
		}
	})
}

// TestSessionLocatorThroughFramework: framework.WithAuthSessionLocator
// reaches the auth routes' Service, which asks it at sign-in with the
// client's full address; a nil one is refused.
func TestSessionLocatorThroughFramework(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	if _, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithAuthSessionLocator(nil)); err == nil {
		t.Fatal("framework.New(WithAuthSessionLocator(nil)) succeeded, want an error")
	}
	var asked netip.Addr
	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithDatabase(db),
		framework.WithAuthSessionLocator(func(_ context.Context, addr netip.Addr) string {
			asked = addr
			return "Lisbon, PT"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	registerUser(t, app, "located@example.com", testPassword)
	rec := sendFrom(app, nil, http.MethodPost, "/api/v1/auth/login", `{"email":"located@example.com","password":"`+testPassword+`"}`, "", "203.0.113.7:51000", "curl/8.5.0")
	var login dataBody[tokenPair]
	if err := json.Unmarshal(rec.Body.Bytes(), &login); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("login = %d, %v: %s", rec.Code, err, rec.Body.String())
	}
	rec = sendFrom(app, nil, http.MethodGet, "/api/v1/auth/sessions", "", login.Data.AccessToken, "203.0.113.7:51000", "curl/8.5.0")
	if !strings.Contains(rec.Body.String(), `"location":"Lisbon, PT"`) || asked != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("sessions = %s, locator asked %v; want the locator's location, asked for 203.0.113.7", rec.Body.String(), asked)
	}
}
