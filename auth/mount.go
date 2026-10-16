package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gombit-dev/gombit/config"
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
// cookie/CSRF threat model.
func Mount(api huma.API, db *gorm.DB, cfg config.Config) error {
	if api == nil {
		return errors.New("auth: nil API")
	}
	if !cfg.Auth.Enabled() {
		return nil
	}
	svc, err := NewService(db, cfg)
	if err != nil {
		return err
	}
	prefix := strings.TrimSuffix(cfg.API.Prefix, "/")
	if prefix == "" {
		prefix = "/api/v1"
	}

	huma.Register(api, huma.Operation{
		OperationID: "register",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/register",
		Summary:     "Register a user",
		Tags:        []string{"Auth"},
	}, svc.register)

	if cfg.Auth.EffectiveMode() == config.AuthModeCookie {
		mountCookieRoutes(api, svc, prefix)
		return nil
	}
	mountBearerRoutes(api, svc, prefix)
	return nil
}

func mountBearerRoutes(api huma.API, svc *Service, prefix string) {
	installBearerScheme(api)

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/login",
		Summary:     "Log in and issue Bearer tokens",
		Tags:        []string{"Auth"},
	}, svc.login)

	huma.Register(api, huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/refresh",
		Summary:     "Rotate the refresh token and issue a new access token",
		Tags:        []string{"Auth"},
	}, svc.refresh)

	huma.Register(api, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/logout",
		Summary:     "Revoke the current refresh token",
		Tags:        []string{"Auth"},
	}, svc.logout)

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

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/login",
		Summary:     "Log in and start an HttpOnly cookie session",
		Tags:        []string{"Auth"},
	}, svc.loginCookie)

	huma.Register(api, huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/refresh",
		Summary:     "Rotate the session cookies",
		Tags:        []string{"Auth"},
	}, svc.refreshCookie)

	huma.Register(api, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        prefix + "/auth/logout",
		Summary:     "Revoke the current session and clear cookies",
		Tags:        []string{"Auth"},
	}, svc.logoutCookie)

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
