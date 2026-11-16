package auth

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/database"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// External identity (AUTH-10): signing in with an account at an external
// identity provider (OIDC or plain OAuth2). The boundary is
// IdentityProvider: an adapter builds the provider's authorization URL and
// turns the code it returns into an ExternalIdentity. Everything else is the
// framework's: the state, the nonce and the PKCE verifier (generated here,
// held in an encrypted single-flow cookie, checked on the callback), the
// mapping of the identity to a local user (IdentityLink), signup, linking,
// and the session the sign-in starts. No provider ships in core; auth/oidc
// is an optional adapter for any OpenID Connect provider.

// IdentityProvider is an external identity provider's adapter. The
// framework calls AuthCodeURL to send the user to the provider, then
// Exchange with the code the provider sends back.
//
// The framework owns the request's state, nonce and PKCE pair: the adapter
// puts them in the authorization URL and the token request as its protocol
// says, and checks nothing of the state (the framework did, before Exchange).
// An OIDC adapter must verify the ID token (issuer, audience, signature,
// expiry) and its nonce against ExchangeRequest.Nonce, return the token's
// nonce in ExternalIdentity.Nonce, and declare itself an IDTokenProvider:
// the framework then refuses an identity whose nonce is missing or not the
// request's. A plain OAuth2 adapter (no ID token) leaves Nonce empty, and
// relies on the state and PKCE.
type IdentityProvider interface {
	// Name is the provider's stable name, in the routes' paths and the
	// stored links ("google", "okta"): 1 to 32 lowercase letters, digits,
	// '-' or '_'. Renaming a provider orphans its links.
	Name() string
	// AuthCodeURL is the provider's authorization URL for req.
	AuthCodeURL(req AuthorizationRequest) (string, error)
	// Exchange redeems the authorization code for the user's identity.
	Exchange(ctx context.Context, req ExchangeRequest) (ExternalIdentity, error)
}

// IDTokenProvider is implemented by an adapter whose identities come from an
// OpenID Connect ID token (auth/oidc's). One whose IssuesIDTokens is true
// declares that each identity carries the token's nonce, and the framework
// holds it to that: an identity with no nonce, or another flow's, is
// refused. Without the declaration the framework can only refuse a nonce
// that is returned and wrong: it cannot tell a plain OAuth2 adapter (no ID
// token, no nonce) from an OIDC adapter that forgot to check and return it.
type IDTokenProvider interface {
	IssuesIDTokens() bool
}

// AuthorizationRequest is what the authorization URL carries.
type AuthorizationRequest struct {
	// State is the opaque value the provider must send back unchanged.
	State string
	// Nonce goes in an OIDC authentication request (nonce) and comes back in
	// the ID token.
	Nonce string
	// CodeChallenge is the PKCE challenge (RFC 7636), with
	// CodeChallengeMethod "S256".
	CodeChallenge       string
	CodeChallengeMethod string
}

// ExchangeRequest is what Exchange redeems.
type ExchangeRequest struct {
	// Code is the authorization code the provider returned.
	Code string
	// CodeVerifier is the PKCE verifier of AuthorizationRequest's challenge.
	CodeVerifier string
	// Nonce is the nonce AuthorizationRequest carried.
	Nonce string
}

// ExternalIdentity is the user as the provider vouches for them.
type ExternalIdentity struct {
	// Subject is the provider's stable, unique id for the user (OIDC sub):
	// never an email, which can change hands. Required; at most 255 bytes.
	Subject string
	// Email is the user's address at the provider, if it shares one.
	Email string
	// EmailVerified is whether the provider verified Email. Only a verified
	// email makes an account.
	EmailVerified bool
	// Nonce is the ID token's nonce (OIDC); empty for plain OAuth2.
	Nonce string
}

// IdentityLink maps an identity at a provider to a local user: who signs
// in with it. One identity (provider, subject) links one user; a user may
// link several.
type IdentityLink struct {
	ID       uint   `gorm:"primaryKey"`
	UserID   uint   `gorm:"index;not null"`
	Provider string `gorm:"size:32;not null;uniqueIndex:idx_auth_identity_links_provider_subject,priority:1"`
	// SubjectKey is the SHA-256 (hex) of Subject: what identifies the link.
	// The subject is compared byte for byte (an OIDC sub is case-sensitive),
	// and a column compared under a collation that ignores case or trailing
	// spaces (MySQL's default) would let two users' subjects meet.
	SubjectKey string `gorm:"size:64;not null;uniqueIndex:idx_auth_identity_links_provider_subject,priority:2"`
	// Subject is the provider's id for the user, as it sent it.
	Subject string `gorm:"size:255;not null"`
	// Email is the identity's address at the provider when it last signed
	// in, for display: it decides nothing.
	Email string `gorm:"size:255;not null;default:''"`
	// LastUsedAt is the identity's latest sign-in; nil until one.
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// TableName keeps the table in the auth_ namespace.
func (IdentityLink) TableName() string { return "auth_identity_links" }

// PurposeExternalLogin is a Bearer-mode external sign-in's exchange code: the
// callback redirects with it, and POST /auth/external/exchange trades it
// for the session's tokens (AUTH-10). Never mailed.
const PurposeExternalLogin TokenPurpose = "external_login"

// unusablePasswordPrefix marks a PasswordHash that no password matches: an
// account an external sign-in made. A password reset gives it one.
const unusablePasswordPrefix = "!"

// HasUsablePassword reports whether the user can sign in with a password:
// false for an account an external sign-in made, until a password reset
// sets one.
func (u User) HasUsablePassword() bool {
	return u.PasswordHash != "" && !strings.HasPrefix(u.PasswordHash, unusablePasswordPrefix)
}

const (
	// externalFlowTTL bounds a flow, from its start to the callback.
	externalFlowTTL = 10 * time.Minute
	// externalCodeTTL bounds a Bearer-mode exchange code.
	externalCodeTTL = 2 * time.Minute
	// externalExchangeTimeout bounds the adapter's Exchange.
	externalExchangeTimeout = 20 * time.Second
	// ExternalStateCookieName holds the flow's encrypted state, between the
	// start of an external sign-in and its callback.
	ExternalStateCookieName = "gombit_external"
	// ExternalCodeCookieName holds a Bearer-mode code's browser binding,
	// between the callback and the exchange.
	ExternalCodeCookieName = "gombit_external_code"
)

var providerNamePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// reservedProviderNames are the routes' own path segments.
var reservedProviderNames = map[string]bool{"identities": true, "exchange": true}

// WithIdentityProviders registers external identity providers: with at least
// one, the routes mount /auth/external/*. Names must be valid and distinct
// (NewService fails otherwise).
func WithIdentityProviders(providers ...IdentityProvider) Option {
	return func(s *Service) {
		for _, p := range providers {
			if p == nil {
				s.providerErr = errors.Join(s.providerErr, errors.New("auth: nil identity provider"))
				continue
			}
			name := p.Name()
			switch {
			case !providerNamePattern.MatchString(name):
				s.providerErr = errors.Join(s.providerErr, fmt.Errorf("auth: identity provider name %q: want 1 to 32 of a-z, 0-9, '-', '_'", name))
				continue
			case reservedProviderNames[name]:
				s.providerErr = errors.Join(s.providerErr, fmt.Errorf("auth: identity provider name %q is reserved", name))
				continue
			case s.providers[name] != nil:
				s.providerErr = errors.Join(s.providerErr, fmt.Errorf("auth: identity provider %q registered twice", name))
				continue
			}
			if s.providers == nil {
				s.providers = map[string]IdentityProvider{}
			}
			s.providers[name] = p
			s.providerNames = append(s.providerNames, name)
		}
	}
}

// WithoutExternalSignup stops an external sign-in from making accounts: an
// identity no user linked is refused (signup_disabled), and users link
// identities from their signed-in accounts only.
func WithoutExternalSignup() Option {
	return func(s *Service) { s.externalSignupOff = true }
}

// IdentityProviders returns the registered providers' names, in
// registration order.
func (s *Service) IdentityProviders() []string {
	return append([]string(nil), s.providerNames...)
}

// externalMode is what a flow does on its callback.
type externalMode string

const (
	externalModeLogin externalMode = "login"
	externalModeLink  externalMode = "link"
)

// externalState is a flow's state, sealed in the state cookie.
type externalState struct {
	Provider string       `json:"p"`
	State    string       `json:"s"`
	Nonce    string       `json:"n"`
	Verifier string       `json:"v"`
	Mode     externalMode `json:"m"`
	// UserID and SessionID are a link's user and the session that asked
	// for it (the refresh token row the gate authenticated).
	UserID    uint   `json:"u,omitempty"`
	SessionID uint   `json:"r,omitempty"`
	ReturnTo  string `json:"t"`
	Expires   int64  `json:"e"`
}

// externalStart is a started flow: where to send the user, and the state
// cookie's value.
type externalStart struct {
	URL    string
	Sealed string
}

// The external flow's refusals. Each reaches the app as its gombit_error
// code (externalErrorCode).
var (
	errUnknownProvider         = errors.New("auth: unknown identity provider")
	errExternalState           = errors.New("auth: invalid or expired external sign-in state")
	errExternalIdentity        = errors.New("auth: the provider returned an invalid identity")
	errExternalAccountExists   = errors.New("auth: an account has the identity's email; link the identity from it")
	errExternalEmailUnverified = errors.New("auth: the provider did not share a verified email")
	errExternalSignupDisabled  = errors.New("auth: the identity is not linked to an account")
	errExternalMFA             = errors.New("auth: an account with multi-factor authentication signs in with its password")
	errIdentityLinkedElsewhere = errors.New("auth: the identity is linked to another account")
	errPasswordRequiredToLink  = errors.New("auth: an account without a password links no identity; a password reset gives it one")
	errLastSignInMethod        = errors.New("auth: the identity is the account's only way to sign in")
	errIdentityLinkNotFound    = errors.New("auth: identity link not found")
	errInvalidReturnTo         = errors.New("auth: return_to must be a path on this site")
)

// ErrLastSignInMethod is UnlinkIdentity's refusal to remove the only way an
// account without a password signs in.
var ErrLastSignInMethod = errLastSignInMethod

// ErrIdentityLinkNotFound is UnlinkIdentity's link that is not the user's.
var ErrIdentityLinkNotFound = errIdentityLinkNotFound

// beginExternal starts a flow with provider: the state, nonce and PKCE pair,
// sealed for the state cookie, and the provider's authorization URL.
func (s *Service) beginExternal(provider string, st externalState) (externalStart, error) {
	p := s.providers[provider]
	if p == nil {
		return externalStart{}, errUnknownProvider
	}
	returnTo, err := cleanReturnTo(st.ReturnTo)
	if err != nil {
		return externalStart{}, err
	}
	st.Provider = provider
	st.ReturnTo = returnTo
	st.Expires = s.now().Add(externalFlowTTL).Unix()
	for _, field := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		if *field, err = newOpaqueToken(); err != nil {
			return externalStart{}, err
		}
	}
	sealed, err := s.sealExternalState(st)
	if err != nil {
		return externalStart{}, err
	}
	challenge := sha256.Sum256([]byte(st.Verifier))
	url, err := p.AuthCodeURL(AuthorizationRequest{
		State:               st.State,
		Nonce:               st.Nonce,
		CodeChallenge:       base64.RawURLEncoding.EncodeToString(challenge[:]),
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		return externalStart{}, fmt.Errorf("auth: identity provider %s: %w", provider, err)
	}
	return externalStart{URL: url, Sealed: sealed}, nil
}

// beginExternalLink starts linking an identity at provider to the signed-in
// user userID, whose session is the refresh token row sessionID. A link is
// a way to sign in that outlives every session, so a session alone does not
// add one: the account proves its password again (current, throttled as a
// password change is). An account without a password (made by an external
// sign-in) links nothing until a password reset gives it one
// (errPasswordRequiredToLink). An account with multi-factor authentication
// cannot sign in externally, so it links nothing either (errExternalMFA).
func (s *Service) beginExternalLink(ctx context.Context, provider string, userID, sessionID uint, current, returnTo string) (externalStart, error) {
	if s.providers[provider] == nil {
		return externalStart{}, errUnknownProvider
	}
	var user User
	if err := s.db.WithContext(ctx).Where("id = ?", userID).Limit(1).Find(&user).Error; err != nil {
		return externalStart{}, err
	}
	if user.ID == 0 {
		return externalStart{}, errUserNotFound
	}
	if !user.HasUsablePassword() {
		return externalStart{}, errPasswordRequiredToLink
	}
	if _, err := s.checkCurrentPassword(ctx, userID, current, AuditIdentityLinkFailed); err != nil {
		return externalStart{}, err
	}
	f, err := confirmedFactor(s.db.WithContext(ctx), userID)
	if err != nil {
		return externalStart{}, err
	}
	if f.ID != 0 {
		return externalStart{}, errExternalMFA
	}
	return s.beginExternal(provider, externalState{Mode: externalModeLink, UserID: userID, SessionID: sessionID, ReturnTo: returnTo})
}

// cleanReturnTo accepts a path on this site ("/", "/settings?tab=1"): not a
// URL, and not a path a browser reads as another host ("//evil", "/\evil").
// Empty is "/".
func cleanReturnTo(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	if len(raw) > 2048 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return "", errInvalidReturnTo
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '#' {
			return "", errInvalidReturnTo
		}
	}
	// Parsed as the callback parses it to add its parameter (withQuery): a
	// value it cannot parse would lose the code or the error.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "", errInvalidReturnTo
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return "", errInvalidReturnTo
	}
	return raw, nil
}

// externalStateKey derives the state cookie's AES-256-GCM key from the JWT
// secret.
func (s *Service) externalStateKey() []byte {
	key, err := hkdf.Key(sha256.New, s.secret, nil, "gombit auth external state v1", 32)
	if err != nil {
		panic(err) // only for a key length sha256 cannot give
	}
	return key
}

// sealExternal encrypts v (JSON) for one of the external flow's cookies:
// the browser can neither read it (the nonce, the PKCE verifier) nor alter
// it. aad names what it is for, so one cookie's value never opens as
// another's.
func (s *Service) sealExternal(aad string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(s.externalStateKey())
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, []byte(aad))), nil
}

// openExternal opens a value sealExternal sealed for aad into v: false for
// one that is missing, altered, or sealed for something else.
func (s *Service) openExternal(aad, sealed string, v any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || sealed == "" {
		return false
	}
	gcm, err := newGCM(s.externalStateKey())
	if err != nil || len(raw) < gcm.NonceSize() {
		return false
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(aad))
	if err != nil {
		return false
	}
	return json.Unmarshal(plain, v) == nil
}

// sealExternalState seals a flow's state for its cookie, bound to its
// provider.
func (s *Service) sealExternalState(st externalState) (string, error) {
	return s.sealExternal("gombit-external-state:"+st.Provider, st)
}

// openExternalState opens the state cookie of a callback for provider, with
// the state it returned: errExternalState for a missing, altered or expired
// cookie, another provider's, or another state.
func (s *Service) openExternalState(provider, sealed, state string) (externalState, error) {
	var st externalState
	if !s.openExternal("gombit-external-state:"+provider, sealed, &st) {
		return externalState{}, errExternalState
	}
	if st.Provider != provider || !s.now().Before(time.Unix(st.Expires, 0)) || state == "" ||
		subtle.ConstantTimeCompare([]byte(st.State), []byte(state)) != 1 {
		return externalState{}, errExternalState
	}
	return st, nil
}

// externalBinding ties a Bearer-mode exchange code to the browser that ran
// the flow: the callback seals it in that browser's code cookie, and the
// code's stored hash covers Secret, so the code alone (in a URL, a log, a
// link an attacker sends) exchanges nothing.
type externalBinding struct {
	Secret   string `json:"b"`
	Provider string `json:"p"`
}

const externalBindingAAD = "gombit-external-code"

// externalResult is a completed callback: the user, and for a sign-in in
// cookie mode the session's tokens, in Bearer mode the exchange code.
type externalResult struct {
	User User
	Pair TokenPair
	Code string
	// Binding is the Bearer-mode code's browser binding (externalBinding).
	Binding externalBinding
}

// completeExternal finishes the flow st with the provider's code: the
// adapter redeems it, the framework checks the identity (subject, nonce),
// then signs the user in or links the identity.
func (s *Service) completeExternal(ctx context.Context, st externalState, code string, bearer bool) (externalResult, error) {
	p := s.providers[st.Provider]
	if p == nil {
		return externalResult{}, errUnknownProvider
	}
	if code == "" {
		return externalResult{}, errExternalState
	}
	xctx, cancel := context.WithTimeout(ctx, externalExchangeTimeout)
	identity, err := p.Exchange(xctx, ExchangeRequest{Code: code, CodeVerifier: st.Verifier, Nonce: st.Nonce})
	cancel()
	if err != nil {
		return externalResult{}, &providerError{provider: st.Provider, err: err}
	}
	idToken, _ := p.(IDTokenProvider)
	if err := checkIdentity(identity, st.Nonce, idToken != nil && idToken.IssuesIDTokens()); err != nil {
		s.logger.Warn("auth: identity provider returned an invalid identity", zap.String("provider", st.Provider), zap.Error(err))
		return externalResult{}, errExternalIdentity
	}
	identity.Email = normalizeEmail(identity.Email)
	if st.Mode == externalModeLink {
		user, err := s.linkIdentity(ctx, st, identity)
		return externalResult{User: user}, err
	}
	return s.externalLogin(ctx, st.Provider, identity, bearer)
}

// providerError is an adapter's failure: the provider refused the code, or
// could not be reached. Logged, never shown.
type providerError struct {
	provider string
	err      error
}

func (e *providerError) Error() string {
	return "auth: identity provider " + e.provider + ": " + e.err.Error()
}

func (e *providerError) Unwrap() error { return e.err }

// subjectKey is IdentityLink.SubjectKey for subject.
func subjectKey(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])
}

// checkIdentity holds an adapter's identity to the boundary's rules: a
// nonce must be the flow's, and is required from an adapter that declares
// ID tokens (fromIDToken).
func checkIdentity(id ExternalIdentity, nonce string, fromIDToken bool) error {
	if id.Subject == "" || len(id.Subject) > 255 {
		return errors.New("subject is empty or longer than 255 bytes")
	}
	if fromIDToken && id.Nonce == "" {
		return errors.New("an ID token provider returned no nonce")
	}
	if id.Nonce != "" && subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(nonce)) != 1 {
		return errors.New("nonce is not the request's")
	}
	if len(id.Email) > 255 {
		return errors.New("email is longer than 255 bytes")
	}
	return nil
}

// externalLogin signs in with identity: the user it is linked to, or a new
// account for it. An identity no user linked whose verified email is an
// account's is refused (errExternalAccountExists): the account's owner links
// it, signed in, so no provider vouching for an address takes over the
// account that has it.
func (s *Service) externalLogin(ctx context.Context, provider string, identity ExternalIdentity, bearer bool) (externalResult, error) {
	var user User
	var code string
	var binding externalBinding
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var link IdentityLink
		if err := tx.Where("provider = ? AND subject_key = ?", provider, subjectKey(identity.Subject)).Limit(1).Find(&link).Error; err != nil {
			return err
		}
		now := s.now()
		if link.ID != 0 {
			locked, err := lockUserTx(tx, link.UserID)
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				// Its user was deleted: the link is stale, and the identity
				// is no user's again.
				if err := tx.Where("id = ?", link.ID).Delete(&IdentityLink{}).Error; err != nil {
					return err
				}
				link = IdentityLink{}
			case err != nil:
				return err
			default:
				user = locked
				if err := tx.Model(&IdentityLink{}).Where("id = ?", link.ID).Updates(map[string]any{"email": identity.Email, "last_used_at": now}).Error; err != nil {
					return err
				}
			}
		}
		if link.ID == 0 {
			var err error
			if user, err = s.externalSignupTx(tx, provider, identity, now); err != nil {
				return err
			}
			created = true
		}
		if user.Disabled() {
			return errAccountDisabled
		}
		f, err := confirmedFactor(tx, user.ID)
		if err != nil {
			return err
		}
		if f.ID != 0 {
			return errExternalMFA
		}
		if bearer {
			if code, err = newOpaqueToken(); err != nil {
				return err
			}
			if binding.Secret, err = newOpaqueToken(); err != nil {
				return err
			}
			binding.Provider = provider
			return tx.Create(&OneTimeToken{
				UserID:    user.ID,
				Purpose:   PurposeExternalLogin,
				TokenHash: hashOpaqueToken(boundCode(code, binding.Secret)),
				Email:     user.Email,
				FromEmail: user.Email,
				ExpiresAt: now.Add(externalCodeTTL),
				SentAt:    &now,
				CreatedAt: now,
			}).Error
		}
		return nil
	})
	if err != nil {
		s.auditExternalFailure(ctx, provider, user.ID, err)
		return externalResult{}, err
	}
	if created {
		s.Audit(ctx, AuditEvent{Action: AuditRegistered, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID), Details: details("provider", provider)})
	}
	if bearer {
		return externalResult{User: user, Code: code, Binding: binding}, nil
	}
	// The session starts as a password login's does, re-checked under the
	// user's lock: a disable, or a factor enabled, since is refused.
	user.mfaGate = mfaGateNoFactor
	user, pair, err := s.startSessionWith(ctx, user, details("method", "external", "provider", provider))
	if errors.Is(err, errMFARequired) {
		err = errExternalMFA
	}
	if err != nil {
		return externalResult{}, err
	}
	return externalResult{User: user, Pair: pair}, nil
}

// externalSignupTx makes the account of an identity no user linked, and
// links it, in tx.
func (s *Service) externalSignupTx(tx *gorm.DB, provider string, identity ExternalIdentity, now time.Time) (User, error) {
	if s.externalSignupOff {
		return User{}, errExternalSignupDisabled
	}
	if !identity.EmailVerified || !validEmail(identity.Email) {
		return User{}, errExternalEmailUnverified
	}
	existing, err := findUserByEmail(tx, identity.Email)
	if err != nil {
		return User{}, err
	}
	if existing.ID != 0 {
		return User{}, errExternalAccountExists
	}
	marker, err := newOpaqueToken()
	if err != nil {
		return User{}, err
	}
	user := User{
		Email:           identity.Email,
		PasswordHash:    unusablePasswordPrefix + marker,
		EmailVerifiedAt: &now,
		VerifiedEmail:   identity.Email,
	}
	if err := tx.Create(&user).Error; err != nil {
		if database.IsUniqueViolation(err) {
			return User{}, errExternalAccountExists
		}
		return User{}, err
	}
	link := IdentityLink{UserID: user.ID, Provider: provider, SubjectKey: subjectKey(identity.Subject), Subject: identity.Subject, Email: identity.Email, LastUsedAt: &now, CreatedAt: now}
	if err := tx.Create(&link).Error; err != nil {
		if database.IsUniqueViolation(err) {
			// Another callback of the same identity linked it first: this
			// one's account is rolled back.
			return User{}, errIdentityLinkedElsewhere
		}
		return User{}, err
	}
	return user, nil
}

// linkIdentity links identity to the flow's user, under the user's lock:
// only while the session that asked is still active, the account enabled
// and without multi-factor authentication. An identity already linked to
// the user is linked again (its email refreshed); one linked to another
// user is refused.
func (s *Service) linkIdentity(ctx context.Context, st externalState, identity ExternalIdentity) (User, error) {
	var user User
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, st.UserID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errSessionEnded
		}
		if err != nil {
			return err
		}
		user = locked
		if locked.Disabled() {
			return errAccountDisabled
		}
		now := s.now()
		if _, err := requesterChain(tx, locked.ID, st.SessionID, now); err != nil {
			return err
		}
		f, err := confirmedFactor(tx, locked.ID)
		if err != nil {
			return err
		}
		if f.ID != 0 {
			return errExternalMFA
		}
		var link IdentityLink
		if err := tx.Where("provider = ? AND subject_key = ?", st.Provider, subjectKey(identity.Subject)).Limit(1).Find(&link).Error; err != nil {
			return err
		}
		if link.ID != 0 {
			if link.UserID != locked.ID {
				return errIdentityLinkedElsewhere
			}
			return tx.Model(&IdentityLink{}).Where("id = ?", link.ID).Update("email", identity.Email).Error
		}
		link = IdentityLink{UserID: locked.ID, Provider: st.Provider, SubjectKey: subjectKey(identity.Subject), Subject: identity.Subject, Email: identity.Email, CreatedAt: now}
		if err := tx.Create(&link).Error; err != nil {
			if database.IsUniqueViolation(err) {
				return errIdentityLinkedElsewhere
			}
			return err
		}
		return nil
	})
	if err != nil {
		s.Audit(ctx, AuditEvent{Action: AuditIdentityLinkFailed, ActorUserID: uintPtr(st.UserID), TargetUserID: uintPtr(st.UserID), Details: details("provider", st.Provider, "reason", externalErrorCode(err))})
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditIdentityLinked, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID), Details: details("provider", st.Provider)})
	return user, nil
}

// auditExternalFailure records an external sign-in that was refused.
func (s *Service) auditExternalFailure(ctx context.Context, provider string, userID uint, err error) {
	s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(userID), Details: details("method", "external", "provider", provider, "reason", externalErrorCode(err))})
}

// boundCode is what a Bearer-mode code's stored hash covers: the code and
// its browser binding's secret.
func boundCode(code, secret string) string { return code + "." + secret }

// exchangeExternalCode trades a Bearer-mode exchange code for the session's
// tokens: once, within externalCodeTTL, only with the binding the callback
// gave the flow's browser. Ending the user's sessions (a sign-out
// everywhere, a password reset or change, a disable, enabling multi-factor
// authentication) voids the codes not yet exchanged, so a code whose account
// changed so since the callback is invalid like a used one. The session
// start re-checks the factor under the user's lock only as a guard for the
// race of an enrollment between the consume and that start (errExternalMFA).
func (s *Service) exchangeExternalCode(ctx context.Context, code string, binding externalBinding) (User, TokenPair, error) {
	type result struct {
		user User
		pair TokenPair
	}
	r, err := throttleTokenUse(ctx, s, func() (result, error) {
		if code == "" || binding.Secret == "" {
			return result{}, errInvalidOneTimeToken
		}
		var user User
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			_, locked, _, err := s.consumeOneTimeToken(tx, boundCode(code, binding.Secret), PurposeExternalLogin)
			user = locked
			return err
		})
		if err != nil {
			return result{}, err
		}
		user.mfaGate = mfaGateNoFactor
		user, pair, err := s.startSessionWith(ctx, user, details("method", "external", "provider", binding.Provider))
		if errors.Is(err, errMFARequired) {
			err = errExternalMFA
		}
		return result{user: user, pair: pair}, err
	})
	return r.user, r.pair, err
}

// ListIdentityLinks returns the user's linked identities, oldest first.
func (s *Service) ListIdentityLinks(ctx context.Context, userID uint) ([]IdentityLink, error) {
	links := []IdentityLink{}
	err := s.db.WithContext(ctx).Where("user_id = ?", userID).Order("id").Find(&links).Error
	return links, err
}

// UnlinkIdentity removes the user's link linkID: ErrIdentityLinkNotFound
// unless it is the user's, ErrLastSignInMethod when the account has no
// password and no other link (it could not sign in again; a password reset
// gives it one).
func (s *Service) UnlinkIdentity(ctx context.Context, userID, linkID uint) error {
	var link IdentityLink
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, userID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.Where("id = ? AND user_id = ?", linkID, userID).Limit(1).Find(&link).Error; err != nil {
			return err
		}
		if link.ID == 0 {
			return errIdentityLinkNotFound
		}
		if !locked.HasUsablePassword() {
			var others int64
			if err := tx.Model(&IdentityLink{}).Where("user_id = ? AND id <> ?", userID, linkID).Count(&others).Error; err != nil {
				return err
			}
			if others == 0 {
				return errLastSignInMethod
			}
		}
		return tx.Where("id = ?", link.ID).Delete(&IdentityLink{}).Error
	})
	if err != nil {
		return err
	}
	s.Audit(ctx, AuditEvent{Action: AuditIdentityUnlinked, ActorUserID: uintPtr(userID), TargetUserID: uintPtr(userID), Details: details("provider", link.Provider)})
	return nil
}

// externalErrorCode is the gombit_error code a refused callback redirects
// with: what the app shows the user. Server failures are server_error.
func externalErrorCode(err error) string {
	var pe *providerError
	switch {
	case errors.As(err, &pe):
		return "provider_error"
	case errors.Is(err, errExternalIdentity):
		return "invalid_identity"
	case errors.Is(err, errExternalAccountExists):
		return "account_exists"
	case errors.Is(err, errExternalEmailUnverified):
		return "email_unverified"
	case errors.Is(err, errExternalSignupDisabled):
		return "signup_disabled"
	case errors.Is(err, errExternalMFA):
		return "mfa_enabled"
	case errors.Is(err, errAccountDisabled):
		return "account_disabled"
	case errors.Is(err, errIdentityLinkedElsewhere):
		return "identity_linked_elsewhere"
	case errors.Is(err, errSessionEnded):
		return "session_ended"
	case errors.Is(err, errInvalidCredentials), errors.Is(err, errLoginAccountGone):
		return "login_failed"
	default:
		return "server_error"
	}
}
