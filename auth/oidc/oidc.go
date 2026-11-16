// Package oidc is an optional auth.IdentityProvider for any OpenID Connect
// provider (Google, Microsoft Entra ID, Okta, Auth0, Keycloak, ...): the
// authorization code flow with PKCE, the provider's endpoints and keys from
// its discovery document, and the ID token verified (signature, issuer,
// audience, expiry, nonce) before its claims become the
// auth.ExternalIdentity.
//
// Core auth ships no provider: an app imports this package, or writes its
// own adapter against auth.IdentityProvider, and registers it with
// framework.WithAuthIdentityProviders (or auth.WithIdentityProviders).
//
//	google, err := oidc.New(ctx, oidc.Config{
//		Name:         "google",
//		Issuer:       "https://accounts.google.com",
//		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
//		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
//		RedirectURL:  "https://app.example.com/api/v1/auth/external/google/callback",
//	})
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/gombit-dev/gombit/auth"
	"golang.org/x/oauth2"
)

// defaultTimeout bounds each request of the default HTTP client.
const defaultTimeout = 15 * time.Second

// Config configures a Provider.
type Config struct {
	// Name is the provider's name in the routes' paths and the stored
	// links (auth.IdentityProvider.Name).
	Name string
	// Issuer is the provider's issuer URL; New reads its discovery document
	// (Issuer + "/.well-known/openid-configuration").
	Issuer string
	// ClientID and ClientSecret are the app's client at the provider. A
	// public client (PKCE only) has no secret.
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback route's absolute URL, as registered with
	// the provider: {public origin}{API prefix}/auth/external/{Name}/callback.
	RedirectURL string
	// Scopes are requested besides "openid". Default: email, profile.
	Scopes []string
	// HTTPClient makes the discovery, key and token requests. Default: a
	// client with a 15-second timeout.
	HTTPClient *http.Client
}

// Provider is an OpenID Connect auth.IdentityProvider.
type Provider struct {
	name     string
	oauth    oauth2.Config
	verifier *gooidc.IDTokenVerifier
	client   *http.Client
}

var _ auth.IdentityProvider = (*Provider)(nil)

// New reads the issuer's discovery document and returns its Provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	switch {
	case cfg.Name == "":
		return nil, errors.New("oidc: Name is required")
	case cfg.Issuer == "":
		return nil, errors.New("oidc: Issuer is required")
	case cfg.ClientID == "":
		return nil, errors.New("oidc: ClientID is required")
	case cfg.RedirectURL == "":
		return nil, errors.New("oidc: RedirectURL is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	discovered, err := gooidc.NewProvider(gooidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: %s: discovery: %w", cfg.Name, err)
	}
	scopes := cfg.Scopes
	if scopes == nil {
		scopes = []string{"email", "profile"}
	}
	all := []string{gooidc.ScopeOpenID}
	for _, s := range scopes {
		if s != gooidc.ScopeOpenID {
			all = append(all, s)
		}
	}
	return &Provider{
		name: cfg.Name,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     discovered.Endpoint(),
			Scopes:       all,
		},
		verifier: discovered.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		client:   client,
	}, nil
}

// Name implements auth.IdentityProvider.
func (p *Provider) Name() string { return p.name }

// AuthCodeURL implements auth.IdentityProvider: the authorization code
// request, with the state, the nonce and the PKCE challenge.
func (p *Provider) AuthCodeURL(req auth.AuthorizationRequest) (string, error) {
	if req.State == "" || req.Nonce == "" || req.CodeChallenge == "" {
		return "", errors.New("oidc: the request needs a state, a nonce and a PKCE challenge")
	}
	return p.oauth.AuthCodeURL(req.State,
		gooidc.Nonce(req.Nonce),
		oauth2.SetAuthURLParam("code_challenge", req.CodeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", req.CodeChallengeMethod),
	), nil
}

// Exchange implements auth.IdentityProvider: it redeems the code (with the
// PKCE verifier), verifies the ID token and its nonce, and returns its
// subject and email.
func (p *Provider) Exchange(ctx context.Context, req auth.ExchangeRequest) (auth.ExternalIdentity, error) {
	ctx = gooidc.ClientContext(ctx, p.client)
	token, err := p.oauth.Exchange(ctx, req.Code, oauth2.VerifierOption(req.CodeVerifier))
	if err != nil {
		return auth.ExternalIdentity{}, fmt.Errorf("oidc: token request: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return auth.ExternalIdentity{}, errors.New("oidc: the token response has no id_token")
	}
	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return auth.ExternalIdentity{}, fmt.Errorf("oidc: id_token: %w", err)
	}
	if req.Nonce == "" || idToken.Nonce != req.Nonce {
		return auth.ExternalIdentity{}, errors.New("oidc: id_token: the nonce is not the request's")
	}
	var claims struct {
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return auth.ExternalIdentity{}, fmt.Errorf("oidc: id_token claims: %w", err)
	}
	return auth.ExternalIdentity{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.Email != "" && verifiedClaim(claims.EmailVerified),
		Nonce:         idToken.Nonce,
	}, nil
}

// verifiedClaim reads email_verified: a boolean, or the string "true" some
// providers send.
func verifiedClaim(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.EqualFold(s, "true")
	}
	return false
}
