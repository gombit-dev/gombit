package auth

import (
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/danielgtaylor/huma/v2"
	"gorm.io/gorm"
)

const (
	bearerSecurityName = "bearerAuth"
	cookieSecurityName = "cookieAuth"
)

// Mount registers framework-owned auth Huma routes on api. The route shapes
// differ by cfg.Auth.EffectiveMode(): AuthModeJWT (default) mounts the
// Bearer token surface (register/login/refresh/logout/me exchanging JSON
// tokens); AuthModeCookie mounts the same operation IDs and paths but
// exchanges HttpOnly session cookies instead, plus GET /auth/csrf. The two
// modes are mutually exclusive per app — see docs/auth-cookie.md for the
// cookie/CSRF threat model. Both modes mount email verification (AUTH-1)
// and password reset (AUTH-2); opts configure the Service the routes use
// (WithMailer, WithLogger, WithAuditor).
//
// Mount returns the Service the routes use, built from cfg (so the routes
// and the Service share one configuration), and the caller owns it: the
// Service runs background work (password reset messages) that Close
// drains and cancels, and the caller closes it on shutdown, before what
// the work uses (the database, the mailer). framework.App does. With auth
// disabled, Mount mounts nothing and returns a nil Service.
func Mount(api huma.API, db *gorm.DB, cfg config.Config, opts ...Option) (*Service, error) {
	if api == nil {
		return nil, errors.New("auth: nil API")
	}
	if !cfg.Auth.Enabled() {
		return nil, nil
	}
	svc, err := NewService(db, cfg, opts...)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(cfg.API.Prefix, "/")
	if prefix == "" {
		prefix = "/api/v1"
	}

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "register",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/register",
		Summary:     "Register a user",
		Tags:        []string{"Auth"},
	}), svc.register)

	if cfg.Auth.EffectiveMode() == config.AuthModeCookie {
		mountCookieRoutes(api, svc, prefix)
		return svc, nil
	}
	mountBearerRoutes(api, svc, prefix)
	return svc, nil
}

func mountBearerRoutes(api huma.API, svc *Service, prefix string) {
	installBearerScheme(api)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/login",
		Summary:     "Log in and issue Bearer tokens",
		Tags:        []string{"Auth"},
	}), svc.login)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/refresh",
		Summary:     "Rotate the refresh token and issue a new access token",
		Tags:        []string{"Auth"},
	}), svc.refresh)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/logout",
		Summary:     "Revoke the current refresh token",
		Tags:        []string{"Auth"},
	}), svc.logout)

	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        prefix + "/me",
		Summary:     "Return the authenticated user",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{bearerSecurityName: {}}},
		Middlewares: huma.Middlewares{svc.requireBearer()},
	}, svc.me)

	registerSessionRoutes(api, svc, prefix, bearerSecurityName, svc.requireBearer(), false)
	registerVerificationRoutes(api, svc, prefix, bearerSecurityName, svc.requireBearer())
	registerPasswordResetRoutes(api, svc, prefix, false)
	registerPasswordChangeRoutes(api, svc, prefix, bearerSecurityName, svc.requireBearer())
}

func mountCookieRoutes(api huma.API, svc *Service, prefix string) {
	installCookieScheme(api)

	huma.Register(api, huma.Operation{
		OperationID: "csrf",
		Method:      http.MethodGet,
		Path:        prefix + "/auth/csrf",
		Summary:     "Issue a CSRF token to double-submit on state-changing requests",
		Tags:        []string{"Auth"},
	}, svc.issueCSRFToken)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/login",
		Summary:     "Log in and start an HttpOnly cookie session",
		Tags:        []string{"Auth"},
	}), svc.loginCookie)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/refresh",
		Summary:     "Rotate the session cookies",
		Tags:        []string{"Auth"},
		Responses:   cookieRefreshErrorResponses(api),
	}), svc.refreshCookie)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/logout",
		Summary:     "Revoke the current session and clear cookies",
		Tags:        []string{"Auth"},
	}), svc.logoutCookie)

	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        prefix + "/me",
		Summary:     "Return the authenticated user",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{cookieSecurityName: {}}},
		Middlewares: huma.Middlewares{svc.requireCookieSession()},
	}, svc.me)

	registerSessionRoutes(api, svc, prefix, cookieSecurityName, svc.requireCookieSession(), true)
	registerVerificationRoutes(api, svc, prefix, cookieSecurityName, svc.requireCookieSession())
	registerPasswordResetRoutes(api, svc, prefix, true)
	registerPasswordChangeRoutes(api, svc, prefix, cookieSecurityName, svc.requireCookieSession())
}

// cookieRefreshErrorResponses declares the cookie refresh's error responses.
// A 401 also clears the session cookies (rejectRefreshCookie); Huma does not
// document headers attached to an error at runtime, so the 401 is declared
// with its Set-Cookie header. Huma adds its default error response only to an
// operation that declares no other response besides the success one, so the
// default is declared here too, as Huma would. Both carry the D10 envelope.
func cookieRefreshErrorResponses(api huma.API) map[string]*huma.Response {
	envelope := api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(contract.ErrorEnvelope{}), true, "Error")
	return map[string]*huma.Response{
		"401": {
			Description: "No refresh cookie, or a refresh token that is unknown, revoked, expired or reused, or whose user is gone.",
			Headers: map[string]*huma.Header{
				"Set-Cookie": {
					Description: "Clears " + AccessCookieName + " and " + RefreshCookieName + " (empty value, Max-Age=0), " +
						"as logout does, unless another rotation already gave the refresh token a successor: " +
						"that rotation's response sets the session's new cookies in the same browser.",
					Schema: &huma.Schema{Type: huma.TypeString},
				},
			},
			Content: map[string]*huma.MediaType{"application/json": {Schema: envelope}},
		},
		"default": {
			Description: "Error",
			Content:     map[string]*huma.MediaType{"application/json": {Schema: envelope}},
		},
	}
}

func installBearerScheme(api huma.API) {
	openapi := api.OpenAPI()
	if openapi.Components == nil {
		openapi.Components = &huma.Components{}
	}
	if openapi.Components.SecuritySchemes == nil {
		openapi.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	openapi.Components.SecuritySchemes[bearerSecurityName] = &huma.SecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "JWT",
		Description:  "Short-lived access JWT. Send `Authorization: Bearer <token>`. Hold the token in memory only.",
	}
}

func installCookieScheme(api huma.API) {
	openapi := api.OpenAPI()
	if openapi.Components == nil {
		openapi.Components = &huma.Components{}
	}
	if openapi.Components.SecuritySchemes == nil {
		openapi.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	openapi.Components.SecuritySchemes[cookieSecurityName] = &huma.SecurityScheme{
		Type: "apiKey",
		In:   "cookie",
		Name: AccessCookieName,
		Description: "HttpOnly session cookie set by POST /auth/login. State-changing " +
			"requests must also echo the gombit_csrf cookie value as the " +
			"X-CSRF-Token header; see docs/auth-cookie.md.",
	}
}
