package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gin-gonic/gin"
)

const csrfTokenBytes = 32

// CSRFMiddleware enforces the double-submit CSRF defense documented in
// docs/auth-cookie.md for cookie-mode session auth (M5-3). It is a Gin
// middleware (not scoped to Huma auth routes) because CSRF must cover every
// state-changing request in the app, including future feature routes, not
// just the auth surface.
//
// Safe methods (GET/HEAD/OPTIONS) bootstrap a signed CSRF cookie when one is
// missing. Other methods require a X-CSRF-Token header that matches the
// gombit_csrf cookie and verifies against cfg.Auth.JWTSecret; otherwise the
// request is rejected with a D10 403 authorization error.
//
// framework.New wires this into the runtime middleware stack automatically
// when cfg.Auth.Enabled() and cfg.Auth.EffectiveMode() == AuthModeCookie.
//
// exemptPaths are exact request paths that opt out of CSRF enforcement on
// unsafe methods — for non-browser endpoints (webhooks, server-to-server
// callbacks) that cannot participate in the double-submit defense and must
// authenticate themselves by other means (e.g. HMAC signature verification).
// Safe methods still bootstrap the cookie on those paths. See
// framework.WithCSRFExemptPaths and docs/auth-cookie.md.
func CSRFMiddleware(cfg config.Config, exemptPaths ...string) gin.HandlerFunc {
	secret := []byte(cfg.Auth.JWTSecret)
	authCfg := cfg.Auth
	exempt := make(map[string]struct{}, len(exemptPaths))
	for _, path := range exemptPaths {
		if path = strings.TrimSpace(path); path != "" {
			exempt[path] = struct{}{}
		}
	}
	return func(c *gin.Context) {
		if isSafeCSRFMethod(c.Request.Method) {
			ensureCSRFCookie(c, secret, authCfg)
			c.Next()
			return
		}
		if _, ok := exempt[c.Request.URL.Path]; ok {
			c.Next()
			return
		}
		if !validCSRFRequest(c, secret) {
			writeCSRFError(c)
			return
		}
		c.Next()
	}
}

func isSafeCSRFMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func validCSRFRequest(c *gin.Context, secret []byte) bool {
	cookie, err := c.Cookie(CSRFCookieName)
	if err != nil || cookie == "" {
		return false
	}
	header := strings.TrimSpace(c.GetHeader(CSRFHeaderName))
	if header == "" || header != cookie {
		return false
	}
	return validCSRFTokenValue(secret, cookie)
}

func ensureCSRFCookie(c *gin.Context, secret []byte, cfg config.AuthConfig) {
	if existing, err := c.Cookie(CSRFCookieName); err == nil && existing != "" {
		return
	}
	value, err := newCSRFTokenValue(secret)
	if err != nil {
		return
	}
	cookie := csrfCookie(value, cfg)
	http.SetCookie(c.Writer, &cookie)
}

func writeCSRFError(c *gin.Context) {
	env := contract.WithContext(c.Request.Context(), contract.Authorization("csrf token missing or invalid"))
	if env == nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Abort()
	c.Header("Content-Type", "application/json")
	c.Status(env.GetStatus())
	_ = json.NewEncoder(c.Writer).Encode(env)
}

// newCSRFTokenValue generates a fresh random token signed with secret. The
// signed value is used as both the cookie value and the client-echoed
// header/body value, so double-submit verification is a straight comparison
// plus a signature check (cookie-tossing defense: an attacker who cannot
// read cfg.Auth.JWTSecret cannot forge a valid value).
func newCSRFTokenValue(secret []byte) (string, error) {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate csrf token: %w", err)
	}
	token := hex.EncodeToString(buf)
	return token + "." + csrfSignature(secret, token), nil
}

func validCSRFTokenValue(secret []byte, value string) bool {
	token, sig, ok := strings.Cut(value, ".")
	if !ok || token == "" || sig == "" {
		return false
	}
	expected := csrfSignature(secret, token)
	return hmac.Equal([]byte(sig), []byte(expected))
}

func csrfSignature(secret []byte, token string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// csrfResult is the GET /auth/csrf response body. The token mirrors the
// gombit_csrf cookie value exactly so the SPA can read it from the JSON
// body instead of parsing document.cookie.
type csrfResult struct {
	CSRFToken string `json:"csrf_token" example:"3a1c...b2.9e4f...c1" doc:"Anti-CSRF token. Send verbatim as X-CSRF-Token on POST/PUT/PATCH/DELETE requests."`
}

type csrfOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[csrfResult]
}

// issueCSRFToken mounts GET /auth/csrf: the bootstrap endpoint an SPA calls
// once (e.g. on app load) so the first mutating request — including
// /auth/login — has a CSRF cookie to double-submit against.
func (s *Service) issueCSRFToken(ctx context.Context, _ *struct{}) (*csrfOutput, error) {
	value, err := newCSRFTokenValue(s.secret)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("generate csrf token"))
	}
	return &csrfOutput{
		SetCookie: []http.Cookie{csrfCookie(value, s.cfg)},
		Body:      contract.Data[csrfResult]{Data: csrfResult{CSRFToken: value}},
	}, nil
}
