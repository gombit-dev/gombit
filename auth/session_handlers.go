package auth

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
)

// Session-management routes (AUTH-5). Both auth modes register the same
// paths and operation IDs; cookie mode additionally clears the session
// cookies when a request ends its own session.

const (
	revokeScopeOthers = "others"
	revokeScopeAll    = "all"
)

type listSessionsOutput struct {
	Body contract.Data[[]AuthSession]
}

type revokeSessionInput struct {
	ID string `path:"id" minLength:"1" maxLength:"64" doc:"Session id from GET /auth/sessions"`
}

type revokeSessionsInput struct {
	Scope string `query:"scope" required:"true" enum:"others,all" doc:"others ends every session except the current one; all ends every session, the current one included"`
}

// authRevokeResult is named for its schema, like AuthSession.
type authRevokeResult struct {
	OK bool `json:"ok" example:"true" doc:"True when the sessions were revoked"`
}

type revokeOutput struct {
	Body contract.Data[authRevokeResult]
}

type cookieRevokeOutput struct {
	SetCookie []http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[authRevokeResult]
}

func registerSessionRoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context)), cookieMode bool) {
	secured := []map[string][]string{{security: {}}}

	huma.Register(api, huma.Operation{
		OperationID: "auth-list-sessions",
		Method:      http.MethodGet,
		Path:        prefix + "/auth/sessions",
		Summary:     "List the user's active sessions",
		Tags:        []string{"Auth"},
		Security:    secured,
		Middlewares: huma.Middlewares{gate},
	}, svc.listSessions)

	revokeOne := huma.Operation{
		OperationID: "auth-revoke-session",
		Method:      http.MethodDelete,
		Path:        prefix + "/auth/sessions/{id}",
		Summary:     "Revoke one session",
		Description: "Returns 404 when the id names no active session, including one that refreshed after it was listed; list again and retry.",
		Tags:        []string{"Auth"},
		Security:    secured,
		Middlewares: huma.Middlewares{gate},
	}
	revokeMany := huma.Operation{
		OperationID: "auth-revoke-sessions",
		Method:      http.MethodDelete,
		Path:        prefix + "/auth/sessions",
		Summary:     "Revoke every other session, or every session",
		Tags:        []string{"Auth"},
		Security:    secured,
		Middlewares: huma.Middlewares{gate},
	}
	if cookieMode {
		huma.Register(api, revokeOne, svc.revokeSessionCookie)
		huma.Register(api, revokeMany, svc.revokeSessionsCookie)
		return
	}
	huma.Register(api, revokeOne, svc.revokeSession)
	huma.Register(api, revokeMany, svc.revokeSessions)
}

// requestSession returns the authenticated user and the refresh token row id
// of the request's session.
func requestSession(ctx context.Context) (User, uint, error) {
	user, ok := UserFromContext(ctx)
	if !ok {
		return User{}, 0, contract.WithContext(ctx, contract.Authentication("missing credentials"))
	}
	refreshID, ok := sessionFromContext(ctx)
	if !ok {
		return User{}, 0, contract.WithContext(ctx, contract.Authentication("missing credentials"))
	}
	return user, refreshID, nil
}

func (s *Service) listSessions(ctx context.Context, _ *struct{}) (*listSessionsOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	sessions, err := s.ListSessions(ctx, user.ID)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	return &listSessionsOutput{Body: contract.Data[[]AuthSession]{Data: sessions}}, nil
}

// endSession revokes the session id on behalf of the request's session and
// reports whether it was the request's own: the session's row reached along
// the chain from the row the request authenticated as, in the revocation's
// transaction, the row ListSessions marks current (revokeSessionAs).
func (s *Service) endSession(ctx context.Context, id string) (bool, error) {
	user, refreshID, err := requestSession(ctx)
	if err != nil {
		return false, err
	}
	endedCurrent, err := s.revokeSessionAs(ctx, user.ID, refreshID, id)
	if err != nil {
		return false, mapServiceError(ctx, err)
	}
	return endedCurrent, nil
}

// endSessions revokes the sessions scope selects on behalf of the request's
// session and reports whether that included the request's own.
func (s *Service) endSessions(ctx context.Context, scope string) (bool, error) {
	user, refreshID, err := requestSession(ctx)
	if err != nil {
		return false, err
	}
	endsCurrent := scope == revokeScopeAll
	if endsCurrent {
		err = s.revokeAllSessionsAs(ctx, user.ID, refreshID)
	} else {
		err = s.revokeOtherSessions(ctx, user.ID, refreshID)
	}
	if err != nil {
		return false, mapServiceError(ctx, err)
	}
	return endsCurrent, nil
}

func (s *Service) revokeSession(ctx context.Context, input *revokeSessionInput) (*revokeOutput, error) {
	if _, err := s.endSession(ctx, input.ID); err != nil {
		return nil, err
	}
	return &revokeOutput{Body: contract.Data[authRevokeResult]{Data: authRevokeResult{OK: true}}}, nil
}

func (s *Service) revokeSessions(ctx context.Context, input *revokeSessionsInput) (*revokeOutput, error) {
	if _, err := s.endSessions(ctx, input.Scope); err != nil {
		return nil, err
	}
	return &revokeOutput{Body: contract.Data[authRevokeResult]{Data: authRevokeResult{OK: true}}}, nil
}

func (s *Service) revokeSessionCookie(ctx context.Context, input *revokeSessionInput) (*cookieRevokeOutput, error) {
	endedCurrent, err := s.endSession(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	return s.cookieRevokeOutput(endedCurrent), nil
}

func (s *Service) revokeSessionsCookie(ctx context.Context, input *revokeSessionsInput) (*cookieRevokeOutput, error) {
	endedCurrent, err := s.endSessions(ctx, input.Scope)
	if err != nil {
		return nil, err
	}
	return s.cookieRevokeOutput(endedCurrent), nil
}

func (s *Service) cookieRevokeOutput(endedCurrent bool) *cookieRevokeOutput {
	out := &cookieRevokeOutput{Body: contract.Data[authRevokeResult]{Data: authRevokeResult{OK: true}}}
	if endedCurrent {
		out.SetCookie = s.expiredSessionCookies()
	}
	return out
}
