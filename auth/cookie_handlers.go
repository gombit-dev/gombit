package auth

import (
	"context"
	"net/http"

	"github.com/gombit-dev/gombit/contract"
	"github.com/danielgtaylor/huma/v2"
)

// Cookie-mode counterparts to handlers.go's Bearer handlers (M5-3). Instead of
// returning tokens in the JSON body, they set HttpOnly session cookies (see
// cookie.go) and read the refresh/access token back from cookies on
// subsequent requests. See docs/auth-cookie.md for the threat model.

type cookieRefreshInput struct {
	// RefreshToken is populated from the gombit_refresh cookie when present;
	// a missing cookie leaves it as the zero value (empty Value), which the
	// handlers below treat as "no session".
	RefreshToken http.Cookie `cookie:"gombit_refresh"`
}

type cookieLoginInput struct {
	Body loginBody
}

type cookieSessionOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[PublicUser]
}

type cookieLogoutOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[logoutResult]
}

func (s *Service) loginCookie(ctx context.Context, input *cookieLoginInput) (*cookieSessionOutput, error) {
	user, err := s.Authenticate(ctx, input.Body.Email, input.Body.Password)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	pair, err := s.IssueTokens(ctx, user)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("issue tokens"))
	}
	return &cookieSessionOutput{
		SetCookie: s.sessionCookies(pair),
		Body:      contract.Data[PublicUser]{Data: toPublicUser(user)},
	}, nil
}

func (s *Service) refreshCookie(ctx context.Context, input *cookieRefreshInput) (*cookieSessionOutput, error) {
	if input.RefreshToken.Value == "" {
		return nil, contract.WithContext(ctx, contract.Authentication("missing session cookie"))
	}
	pair, err := s.RotateRefresh(ctx, input.RefreshToken.Value)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	user, err := s.ParseAccess(ctx, pair.AccessToken)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("load session user"))
	}
	return &cookieSessionOutput{
		SetCookie: s.sessionCookies(pair),
		Body:      contract.Data[PublicUser]{Data: toPublicUser(user)},
	}, nil
}

func (s *Service) logoutCookie(ctx context.Context, input *cookieRefreshInput) (*cookieLogoutOutput, error) {
	if input.RefreshToken.Value != "" {
		if err := s.RevokeRefresh(ctx, input.RefreshToken.Value); err != nil {
			return nil, mapServiceError(ctx, err)
		}
	}
	return &cookieLogoutOutput{
		SetCookie: s.expiredSessionCookies(),
		Body:      contract.Data[logoutResult]{Data: logoutResult{OK: true}},
	}, nil
}

// sessionCookies builds the Set-Cookie values for a fresh token pair.
func (s *Service) sessionCookies(pair TokenPair) []http.Cookie {
	return []http.Cookie{
		newSessionCookie(AccessCookieName, pair.AccessToken, s.cfg.AccessTokenTTL, s.cfg),
		newSessionCookie(RefreshCookieName, pair.RefreshToken, s.cfg.RefreshTokenTTL, s.cfg),
	}
}

// expiredSessionCookies clears both session cookies (logout).
func (s *Service) expiredSessionCookies() []http.Cookie {
	return []http.Cookie{
		expiredSessionCookie(AccessCookieName, s.cfg),
		expiredSessionCookie(RefreshCookieName, s.cfg),
	}
}

// RequireCookieSession is the exported cookie-session gate used by runtime
// packages such as admin. It authenticates the gombit_access cookie and
// stores the User on the request context for UserFromContext. It does not
// itself enforce CSRF; CSRFMiddleware (csrf.go) is wired as global Gin
// middleware ahead of it for state-changing requests.
func (s *Service) RequireCookieSession() func(ctx huma.Context, next func(huma.Context)) {
	return s.requireCookieSession()
}

// requireCookieSession is the cookie-mode analogue of requireBearer: it reads
// the access token from the gombit_access cookie instead of the Authorization
// header.
func (s *Service) requireCookieSession() func(ctx huma.Context, next func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		cookie, err := huma.ReadCookie(ctx, AccessCookieName)
		if err != nil || cookie.Value == "" {
			writeCookieAuthError(ctx, contract.WithContext(ctx.Context(), contract.Authentication("missing session cookie")))
			return
		}
		user, refreshID, err := s.parseAccess(ctx.Context(), cookie.Value)
		if err != nil {
			writeCookieAuthError(ctx, contract.WithContext(ctx.Context(), contract.Authentication("invalid session cookie")))
			return
		}
		next(withSession(ctx, user, refreshID))
	}
}
