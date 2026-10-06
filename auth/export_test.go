package auth

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

// SetRotateJoinHook makes fn run whenever a caller joins another caller's
// in-flight refresh rotation (before it waits), so a test can pin callers
// inside the shared rotation. Set it before the service is used.
func SetRotateJoinHook(s *Service, fn func()) { s.onRotateJoin = fn }

// ErrInvalidRefreshToken is the error RotateRefresh returns for a refresh
// token that is unknown, revoked, or expired.
var ErrInvalidRefreshToken = errInvalidRefreshToken

// SetClock replaces the service's time source.
func SetClock(s *Service, c Clock) { s.clock = c }

// SessionIDOf returns the session id the API lists for the session the access
// token belongs to.
func SessionIDOf(ctx context.Context, s *Service, access string) (string, error) {
	_, refreshID, err := s.parseAccess(ctx, access)
	if err != nil {
		return "", err
	}
	return s.sessionID(refreshID), nil
}

// ErrInvalidAccessToken is the error ParseAccess returns for an access token
// that is malformed, expired, or whose session has ended.
var ErrInvalidAccessToken = errInvalidAccessToken

// ErrRefreshReuse is the error RotateRefresh returns for the reuse of an
// already-rotated refresh token.
var ErrRefreshReuse = errRefreshReuse

// AuthenticatedRow is the id of the refresh token row the access token is
// bound to, as the middlewares record it for the request.
func AuthenticatedRow(ctx context.Context, s *Service, access string) (uint, error) {
	_, refreshID, err := s.parseAccess(ctx, access)
	return refreshID, err
}

// RevokeOtherSessionsKeeping revokes every session of the user except the one
// whose chain includes the refresh token row refreshID, the row a request was
// authenticated as (AuthenticatedRow), whatever the session did since: what
// DELETE /auth/sessions?scope=others does.
func RevokeOtherSessionsKeeping(ctx context.Context, s *Service, userID, refreshID uint) error {
	return s.revokeOtherSessions(ctx, userID, refreshID)
}

// RequireBearer is the gate the Bearer-mode auth routes sit behind (the
// cookie-mode one is exported as RequireCookieSession).
func RequireBearer(s *Service) func(huma.Context, func(huma.Context)) {
	return s.requireBearer()
}
