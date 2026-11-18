package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"go.uber.org/zap"
)

// The external identity routes (AUTH-10), mounted under
// {prefix}/auth/external when WithIdentityProviders registered at least one
// provider.

type authExternalProvider struct {
	Name string `json:"name" example:"google" doc:"The provider's name, in its routes' paths"`
}

type externalProvidersOutput struct {
	Body contract.Data[[]authExternalProvider]
}

type authExternalStartBody struct {
	ReturnTo string `json:"return_to,omitempty" maxLength:"2048" example:"/settings" doc:"Where the callback redirects the browser: a path on this site (not a URL; no fragment). Default /."`
}

type authExternalLinkBody struct {
	ReturnTo        string `json:"return_to,omitempty" maxLength:"2048" example:"/settings" doc:"Where the callback redirects the browser: a path on this site (not a URL; no fragment). Default /."`
	CurrentPassword string `json:"current_password" maxLength:"72" doc:"The account's password."`
}

type authExternalAuthorization struct {
	AuthorizationURL string `json:"authorization_url" example:"https://accounts.example.com/authorize?..." doc:"The provider's authorization URL: navigate the browser to it."`
}

type externalStartInput struct {
	Provider string `path:"provider" maxLength:"32" doc:"The provider's name"`
	Body     *authExternalStartBody
}

type externalLinkInput struct {
	Provider string `path:"provider" maxLength:"32" doc:"The provider's name"`
	Body     *authExternalLinkBody
}

type externalStartOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[authExternalAuthorization]
}

type externalCallbackInput struct {
	Provider string `path:"provider" maxLength:"32" doc:"The provider's name"`
	Code     string `query:"code" maxLength:"4096" doc:"The authorization code, from the provider"`
	State    string `query:"state" maxLength:"512" doc:"The flow's state, from the provider"`
	Error    string `query:"error" maxLength:"256" doc:"The provider's error, when it refused (access_denied: the user declined)"`
	// StateCookie is the flow's sealed state.
	StateCookie http.Cookie `cookie:"gombit_external"`
}

type externalRedirectOutput struct {
	Status    int
	Location  string        `header:"Location"`
	SetCookie []http.Cookie `header:"Set-Cookie"`
}

type authExternalExchangeBody struct {
	Code string `json:"code" minLength:"1" maxLength:"128" doc:"The gombit_code the callback redirected with"`
}

type externalExchangeInput struct {
	Body authExternalExchangeBody
	// CodeCookie is the browser binding the callback set with the code.
	CodeCookie http.Cookie `cookie:"gombit_external_code"`
}

type externalExchangeOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      contract.Data[TokenPair]
}

type authIdentityLink struct {
	ID         uint       `json:"id" doc:"The link's id"`
	Provider   string     `json:"provider" example:"google" doc:"The provider's name"`
	Subject    string     `json:"subject" doc:"The user's id at the provider"`
	Email      string     `json:"email" doc:"The identity's email at the provider when it last signed in (display only)"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty" doc:"The identity's latest sign-in"`
	CreatedAt  time.Time  `json:"created_at" doc:"When it was linked"`
}

type externalIdentitiesOutput struct {
	Body contract.Data[[]authIdentityLink]
}

type externalUnlinkInput struct {
	ID uint `path:"id" doc:"The link's id"`
}

func registerExternalRoutes(api huma.API, svc *Service, prefix, security string, gate func(huma.Context, func(huma.Context))) {
	if len(svc.providerNames) == 0 {
		return
	}
	base := prefix + "/auth/external"
	cookieMode := svc.cfg.EffectiveMode() == config.AuthModeCookie

	huma.Register(api, huma.Operation{
		OperationID: "auth-external-providers",
		Method:      http.MethodGet,
		Path:        base,
		Summary:     "List the external identity providers",
		Tags:        []string{"Auth"},
	}, svc.listExternalProviders)

	huma.Register(api, huma.Operation{
		OperationID: "auth-external-login",
		Method:      http.MethodPost,
		Path:        base + "/{provider}/login",
		Summary:     "Start signing in with an external identity provider",
		Description: "Returns the provider's authorization URL, and sets the flow's state cookie (HttpOnly, for the callback's path, ten minutes): navigate this browser to the URL. The callback signs the user in, or makes an account for an identity no user linked when the provider verified its email and no account has it; an email an account has is refused (account_exists): its owner links the identity, signed in. An unknown provider is a 404.",
		Tags:        []string{"Auth"},
	}, svc.startExternalLogin)

	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID: "auth-external-link",
		Method:      http.MethodPost,
		Path:        base + "/{provider}/link",
		Summary:     "Start linking an external identity to the signed-in user",
		Description: "As /login, but the callback links the identity to this account (refused when another account has it) and signs nobody in. Requires the current password (a wrong one is a 422 on current_password; too many are a 429 with Retry-After): a link signs in for good, past every session. An account without a password (made by an external sign-in) links nothing until a password reset gives it one (409), and an account with multi-factor authentication cannot sign in externally, so it links nothing either (409).",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{security: {}}},
		Middlewares: huma.Middlewares{gate},
		Errors:      []int{http.StatusTooManyRequests},
	}), svc.startExternalLink)
	declareRetryAfter(api, base+"/{provider}/link", http.StatusTooManyRequests, throttleRetryAfterDoc)

	callbackDoc := "The provider's redirect URI: register this URL with the provider. Checks the state against the flow's cookie (a 422 on state when it is missing, expired, or not this flow's) and clears it, has the provider redeem the code, then redirects (302) to the flow's return_to. "
	if cookieMode {
		callbackDoc += "A sign-in sets the session cookies. "
	} else {
		callbackDoc += "A sign-in adds gombit_code to return_to's query: trade it for the session's tokens with POST /auth/external/exchange, once, within two minutes, from this browser: the callback also sets it the gombit_external_code cookie (HttpOnly, for the exchange's path) that the exchange needs, so the code alone, in another browser, signs nobody in. "
	}
	callbackDoc += "A refusal adds gombit_error instead: access_denied, provider_error, invalid_identity, account_exists, email_unverified, signup_disabled, mfa_enabled, account_disabled, identity_linked_elsewhere, session_ended, login_failed or server_error."
	huma.Register(api, withRequestInfo(huma.Operation{
		OperationID:   "auth-external-callback",
		Method:        http.MethodGet,
		Path:          base + "/{provider}/callback",
		Summary:       "Finish an external sign-in or link (the provider redirects here)",
		Description:   callbackDoc,
		Tags:          []string{"Auth"},
		DefaultStatus: http.StatusFound,
	}), svc.externalCallback)

	if !cookieMode {
		huma.Register(api, withRequestInfo(huma.Operation{
			OperationID: "auth-external-exchange",
			Method:      http.MethodPost,
			Path:        base + "/exchange",
			Summary:     "Trade an external sign-in's code for Bearer tokens",
			Description: "The code works once, within two minutes of the callback; an unknown, expired or used one is a 422 on code. The code works only from the browser that ran the flow: the callback set it a cookie the exchange needs (send credentials). A code from elsewhere is a 422 on code, and so is one whose account was disabled, given multi-factor authentication, or had its sessions ended (signed out everywhere, a password reset or change) since the callback: those void it. Only an account given multi-factor authentication in the instant between the code's use and the session's start is a 409. Too many invalid codes from one client address is a 429 (Retry-After).",
			Tags:        []string{"Auth"},
			Errors:      []int{http.StatusTooManyRequests},
		}), svc.exchangeExternal)
		declareRetryAfter(api, base+"/exchange", http.StatusTooManyRequests, throttleRetryAfterDoc)
	}

	huma.Register(api, huma.Operation{
		OperationID: "auth-external-identities",
		Method:      http.MethodGet,
		Path:        base + "/identities",
		Summary:     "List the signed-in user's linked external identities",
		Tags:        []string{"Auth"},
		Security:    []map[string][]string{{security: {}}},
		Middlewares: huma.Middlewares{gate},
	}, svc.listExternalIdentities)

	huma.Register(api, huma.Operation{
		OperationID:   "auth-external-unlink",
		Method:        http.MethodDelete,
		Path:          base + "/identities/{id}",
		Summary:       "Remove one of the signed-in user's linked external identities",
		Description:   "A link that is not the user's is a 404. An account without a password cannot remove its only link (409): a password reset gives it one.",
		Tags:          []string{"Auth"},
		Security:      []map[string][]string{{security: {}}},
		Middlewares:   huma.Middlewares{gate},
		DefaultStatus: http.StatusNoContent,
	}, svc.unlinkExternalIdentity)
}

func (s *Service) listExternalProviders(_ context.Context, _ *struct{}) (*externalProvidersOutput, error) {
	out := make([]authExternalProvider, 0, len(s.providerNames))
	for _, name := range s.providerNames {
		out = append(out, authExternalProvider{Name: name})
	}
	return &externalProvidersOutput{Body: contract.Data[[]authExternalProvider]{Data: out}}, nil
}

func (s *Service) startExternalLogin(ctx context.Context, input *externalStartInput) (*externalStartOutput, error) {
	var returnTo string
	if input.Body != nil {
		returnTo = input.Body.ReturnTo
	}
	start, err := s.beginExternal(input.Provider, externalState{Mode: externalModeLogin, ReturnTo: returnTo})
	if err != nil {
		return nil, mapExternalError(ctx, err)
	}
	return s.externalStartOutput(start), nil
}

func (s *Service) startExternalLink(ctx context.Context, input *externalLinkInput) (*externalStartOutput, error) {
	user, rid, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	var body authExternalLinkBody
	if input.Body != nil {
		body = *input.Body
	}
	start, err := s.beginExternalLink(ctx, input.Provider, user.ID, rid, body.CurrentPassword, body.ReturnTo)
	if err != nil {
		return nil, mapExternalError(ctx, err)
	}
	return s.externalStartOutput(start), nil
}

func (s *Service) externalStartOutput(start externalStart) *externalStartOutput {
	return &externalStartOutput{
		SetCookie: s.externalStateCookie(start.Sealed, int(externalFlowTTL.Seconds())),
		Body:      contract.Data[authExternalAuthorization]{Data: authExternalAuthorization{AuthorizationURL: start.URL}},
	}
}

// externalStateCookie is the flow's state cookie (maxAge < 0 clears it).
// Its path is the external routes', so no other request carries it.
func (s *Service) externalStateCookie(value string, maxAge int) http.Cookie {
	return s.externalCookie(ExternalStateCookieName, "/auth/external", value, maxAge)
}

// externalCodeCookie is a Bearer-mode code's browser binding (maxAge < 0
// clears it): sent to the exchange only.
func (s *Service) externalCodeCookie(value string, maxAge int) http.Cookie {
	return s.externalCookie(ExternalCodeCookieName, "/auth/external/exchange", value, maxAge)
}

// externalCookie is one of the external flow's cookies, under path (below
// the API prefix). HttpOnly; SameSite=Lax whatever the session cookies'
// setting, since the provider's redirect back is a cross-site navigation,
// which a Strict cookie misses; Secure with GOMBIT_COOKIE_SECURE, and
// always in production, in either auth mode (the production check of
// GOMBIT_COOKIE_SECURE covers cookie mode only).
func (s *Service) externalCookie(name, path, value string, maxAge int) http.Cookie {
	//nolint:gosec // G124: Secure is CookieSecure, forced on in production (externalCookie's doc); HttpOnly and SameSite are set.
	return http.Cookie{
		Name:     name,
		Value:    value,
		Path:     s.apiPrefix + path,
		MaxAge:   maxAge,
		Secure:   s.cfg.CookieSecure || s.production,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Service) externalCallback(ctx context.Context, input *externalCallbackInput) (*externalRedirectOutput, error) {
	cleared := s.externalStateCookie("", -1)
	st, err := s.openExternalState(input.Provider, input.StateCookie.Value, input.State)
	if err != nil {
		// No flow of this browser's: nowhere to send it back to.
		resp := invalidField(ctx, "state", "invalid or expired external sign-in; start again")
		if input.StateCookie.Value == "" {
			return nil, resp
		}
		return nil, huma.ErrorWithHeaders(resp, http.Header{"Set-Cookie": []string{cleared.String()}})
	}
	redirect := func(param, value string) *externalRedirectOutput {
		cookies := []http.Cookie{cleared}
		return &externalRedirectOutput{Status: http.StatusFound, Location: withQuery(st.ReturnTo, param, value), SetCookie: cookies}
	}
	if input.Error != "" {
		code := "provider_error"
		if input.Error == "access_denied" {
			code = "access_denied"
		}
		return redirect("gombit_error", code), nil
	}
	bearer := s.cfg.EffectiveMode() != config.AuthModeCookie
	res, err := s.completeExternal(ctx, st, input.Code, bearer)
	if err != nil {
		code := externalErrorCode(err)
		if errors.Is(err, errExternalState) {
			code = "provider_error"
		}
		if code == "server_error" || code == "provider_error" {
			s.logger.Warn("auth: external sign-in failed", zap.String("provider", st.Provider), zap.Error(err))
		}
		return redirect("gombit_error", code), nil
	}
	switch {
	case st.Mode == externalModeLink:
		return redirect("gombit_linked", st.Provider), nil
	case bearer:
		// The code reaches the page in its URL; the binding stays in this
		// browser's cookie, and the exchange needs both.
		sealed, err := s.sealExternal(externalBindingAAD, res.Binding)
		if err != nil {
			return redirect("gombit_error", "server_error"), nil
		}
		out := redirect("gombit_code", res.Code)
		out.SetCookie = append(out.SetCookie, s.externalCodeCookie(sealed, int(externalCodeTTL.Seconds())))
		return out, nil
	}
	out := redirect("", "")
	out.SetCookie = append(out.SetCookie, s.sessionCookies(res.Pair)...)
	return out, nil
}

// withQuery adds name=value to path's query (none when name is empty).
func withQuery(path, name, value string) string {
	if name == "" {
		return path
	}
	u, err := url.Parse(path)
	if err != nil {
		return "/"
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Service) exchangeExternal(ctx context.Context, input *externalExchangeInput) (*externalExchangeOutput, error) {
	// The binding works with one code, once: cleared whatever the outcome.
	cleared := s.externalCodeCookie("", -1)
	var binding externalBinding
	if !s.openExternal(externalBindingAAD, input.CodeCookie.Value, &binding) {
		binding = externalBinding{}
	}
	_, pair, err := s.exchangeExternalCode(ctx, input.Body.Code, binding)
	if err != nil {
		resp := mapExternalError(ctx, err)
		if errors.Is(err, errInvalidOneTimeToken) {
			resp = invalidField(ctx, "code", "invalid or expired code, or not this browser's")
		}
		return nil, huma.ErrorWithHeaders(resp, http.Header{"Set-Cookie": []string{cleared.String()}})
	}
	return &externalExchangeOutput{SetCookie: cleared, Body: contract.Data[TokenPair]{Data: pair}}, nil
}

func (s *Service) listExternalIdentities(ctx context.Context, _ *struct{}) (*externalIdentitiesOutput, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	links, err := s.ListIdentityLinks(ctx, user.ID)
	if err != nil {
		return nil, mapServiceError(ctx, err)
	}
	out := make([]authIdentityLink, 0, len(links))
	for _, l := range links {
		out = append(out, authIdentityLink{ID: l.ID, Provider: l.Provider, Subject: l.Subject, Email: l.Email, LastUsedAt: l.LastUsedAt, CreatedAt: l.CreatedAt})
	}
	return &externalIdentitiesOutput{Body: contract.Data[[]authIdentityLink]{Data: out}}, nil
}

func (s *Service) unlinkExternalIdentity(ctx context.Context, input *externalUnlinkInput) (*struct{}, error) {
	user, _, err := requestSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.UnlinkIdentity(ctx, user.ID, input.ID); err != nil {
		return nil, mapExternalError(ctx, err)
	}
	return nil, nil
}

// mapExternalError reports the external routes' own refusals, and the rest
// as mapServiceError does.
func mapExternalError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errUnknownProvider):
		return contract.WithContext(ctx, contract.NotFound("unknown identity provider"))
	case errors.Is(err, errInvalidReturnTo):
		return invalidField(ctx, "return_to", "must be a path on this site, without a fragment")
	case errors.Is(err, errExternalMFA):
		return contract.WithContext(ctx, contract.Conflict("an account with multi-factor authentication signs in with its password, not an external identity"))
	case errors.Is(err, errIdentityLinkNotFound):
		return contract.WithContext(ctx, contract.NotFound("identity link not found"))
	case errors.Is(err, errPasswordRequiredToLink):
		return contract.WithContext(ctx, contract.Conflict("the account has no password: set one with a password reset, then link the identity with it"))
	case errors.Is(err, errLastSignInMethod):
		return contract.WithContext(ctx, contract.Conflict("the account has no password and no other linked identity: it could not sign in again"))
	}
	return mapServiceError(ctx, err)
}
