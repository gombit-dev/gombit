package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Service is the runtime auth implementation: users, password hashes,
// access JWTs, and rotating refresh tokens.
type Service struct {
	db          *gorm.DB
	cfg         config.AuthConfig
	secret      []byte
	hasher      Hasher
	clock       Clock
	mailer      Mailer
	logger      *zap.Logger
	auditor     Auditor
	dummyOnce   sync.Once
	dummyHash   string
	rotateMu    sync.Mutex
	rotateCalls map[string]*rotateCall
	// onRotateJoin, when set (tests only), runs when a caller joins another
	// caller's in-flight rotation, just before it waits for the result.
	onRotateJoin func()
}

type rotateCall struct {
	wg   sync.WaitGroup
	pair TokenPair
	err  error
}

// Option configures a Service (NewService) or the routes Mount registers.
type Option func(*Service)

// WithMailer sets the Mailer the account lifecycle's messages go through
// (email verification). Without one, sending a message fails with
// ErrNoMailer, and registration sends none.
func WithMailer(m Mailer) Option {
	return func(s *Service) {
		if m != nil {
			s.mailer = m
		}
	}
}

// WithLogger sets the logger for what the service reports without failing
// the request (a verification message that could not be sent at
// registration). Default: none.
func WithLogger(l *zap.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService constructs a Service. cfg.Auth.JWTSecret must be set.
func NewService(db *gorm.DB, cfg config.Config, opts ...Option) (*Service, error) {
	if db == nil {
		return nil, errors.New("auth: nil database")
	}
	if !cfg.Auth.Enabled() {
		return nil, errors.New("auth: JWT secret is not set")
	}
	if cfg.Auth.AccessTokenTTL <= 0 || cfg.Auth.RefreshTokenTTL <= 0 {
		return nil, errors.New("auth: token TTLs must be positive")
	}
	if cfg.Auth.RefreshTokenTTL < cfg.Auth.AccessTokenTTL {
		return nil, errors.New("auth: the refresh token TTL must be at least the access token TTL")
	}
	s := &Service{
		db:     db,
		cfg:    cfg.Auth,
		secret: []byte(cfg.Auth.JWTSecret),
		hasher: newBcryptHasher(cfg.Auth.BcryptCost),
		clock:  systemClock{},
		mailer: noMailer{},
		logger: zap.NewNop(),
	}
	s.auditor = DBAuditor(db)
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func (s *Service) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}

// Register creates a user, with an unverified email, and mails them a
// verification link when a Mailer is configured (a failure to send is
// logged, not returned: POST /auth/verify-email/resend asks again).
// Duplicate emails return errEmailTaken.
func (s *Service) Register(ctx context.Context, email, password string) (User, error) {
	user, err := s.createUser(ctx, email, password, false)
	if err != nil {
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditRegistered, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID)})
	s.sendVerificationOnRegister(ctx, user)
	return user, nil
}

// CreateSuperuser creates a user with IsSuperuser set, for gombit
// createsuperuser (M4-6). It shares Register's hasher and uniqueness path;
// duplicate emails return errEmailTaken. The operator who creates it vouches
// for the address, so its email starts verified, and no message is sent.
func (s *Service) CreateSuperuser(ctx context.Context, email, password string) (User, error) {
	user, err := s.createUser(ctx, email, password, true)
	if err != nil {
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditSuperuserCreated, TargetUserID: uintPtr(user.ID)})
	return user, nil
}

func (s *Service) createUser(ctx context.Context, email, password string, superuser bool) (User, error) {
	email = normalizeEmail(email)
	if email == "" {
		return User{}, errInvalidCredentials
	}
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return User{}, err
	}
	user := User{Email: email, PasswordHash: hash, IsSuperuser: superuser}
	if superuser {
		now := s.now()
		user.EmailVerifiedAt = &now
		user.VerifiedEmail = email
	}
	if err := s.db.WithContext(ctx).Create(&user).Error; err != nil {
		if database.IsUniqueViolation(err) {
			return User{}, errEmailTaken
		}
		return User{}, err
	}
	return user, nil
}

// Authenticate verifies email and password.
func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	email = normalizeEmail(email)
	var user User
	err := s.db.WithContext(ctx).Where("email = ?", email).First(&user).Error
	if err != nil {
		s.compareDummy(password)
		// No target: the attempted address is not recorded.
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, Details: details("reason", "unknown_account")})
		return User{}, errInvalidCredentials
	}
	if err := s.hasher.Compare(user.PasswordHash, password); err != nil {
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "wrong_password")})
		return User{}, errInvalidCredentials
	}
	return user, nil
}

// IssueTokens returns a new access JWT and a rotating refresh token: it
// starts a session, so it records the login as succeeded (Authenticate
// records the failures).
func (s *Service) IssueTokens(ctx context.Context, user User) (TokenPair, error) {
	pair, err := s.issueTokens(s.db.WithContext(ctx), user, s.now())
	if err != nil {
		return TokenPair{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditLoginSucceeded, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID)})
	return pair, nil
}

func (s *Service) issueTokens(tx *gorm.DB, user User, now time.Time) (TokenPair, error) {
	raw, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}
	row := RefreshToken{
		UserID:    user.ID,
		TokenHash: hashRefreshToken(raw),
		ExpiresAt: now.Add(s.cfg.RefreshTokenTTL),
		CreatedAt: now,
	}
	if err := tx.Create(&row).Error; err != nil {
		return TokenPair{}, err
	}
	access, err := signAccessToken(s.secret, user, row.ID, s.cfg.AccessTokenTTL, now)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:  access,
		RefreshToken: raw,
		TokenType:    "Bearer",
		ExpiresIn:    int(s.cfg.AccessTokenTTL.Seconds()),
	}, nil
}

// RotateRefresh validates the current refresh token, revokes it, and issues a
// new pair. Reuse of an already-rotated token revokes the user's remaining
// tokens; a token revoked by logout or session revocation is just invalid.
// Concurrent callers with the same still-valid token share one
// rotation so a lost race cannot family-revoke the winner's new session.
func (s *Service) RotateRefresh(ctx context.Context, raw string) (TokenPair, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return TokenPair{}, errInvalidRefreshToken
	}
	hash := hashRefreshToken(raw)

	s.rotateMu.Lock()
	if s.rotateCalls == nil {
		s.rotateCalls = make(map[string]*rotateCall)
	}
	if call, ok := s.rotateCalls[hash]; ok {
		s.rotateMu.Unlock()
		if s.onRotateJoin != nil {
			s.onRotateJoin()
		}
		call.wg.Wait()
		return call.pair, call.err
	}
	call := &rotateCall{}
	call.wg.Add(1)
	s.rotateCalls[hash] = call
	s.rotateMu.Unlock()

	pair, err := s.rotateRefreshOnce(ctx, raw, hash)
	call.pair, call.err = pair, err
	s.rotateMu.Lock()
	delete(s.rotateCalls, hash)
	s.rotateMu.Unlock()
	call.wg.Done()
	return pair, err
}

// rotateRefreshOnce runs one rotation in a transaction. Lock order: the
// user's row (lockUserTx) comes before any write to a refresh_tokens row,
// the same order every revocation uses; the compare-and-swap below must stay
// after it. The initial read of the token takes no lock. The rotation's
// instant is read once the lock is held: the lock can wait behind another
// rotation or a revocation, and a token that expired during that wait must
// not rotate, nor its successor's lifetime start before the wait.
func (s *Service) rotateRefreshOnce(ctx context.Context, raw, hash string) (TokenPair, error) {
	var pair TokenPair
	var reuse bool
	var reuseUser uint
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row RefreshToken
		if err := tx.Where("token_hash = ?", hash).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errInvalidRefreshToken
			}
			return err
		}
		user, err := lockUserTx(tx, row.UserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errUserNotFound
			}
			// A deadlock, lock timeout or lost connection is a server
			// failure, not a dead credential: it must not read as a 401.
			return err
		}
		now := s.now()
		if row.RevokedAt != nil {
			if row.ReplacedBy == nil {
				// Revoked by logout or session revocation, not by rotation:
				// presenting it again is a dead credential, not evidence of
				// theft, so it must not end the user's other sessions.
				return errInvalidRefreshToken
			}
			if err := revokeAllTx(tx, row.UserID, now); err != nil {
				return err
			}
			reuse, reuseUser = true, row.UserID
			return nil
		}
		if !row.ExpiresAt.After(now) {
			return errInvalidRefreshToken
		}

		result := tx.Model(&RefreshToken{}).
			Where("id = ? AND revoked_at IS NULL", row.ID).
			Updates(map[string]any{"revoked_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			// Lost the compare-and-swap: another caller already rotated this
			// still-valid token. That is not reuse-as-theft; do not revoke the
			// winner's new session.
			return errInvalidRefreshToken
		}

		issued, err := s.issueTokens(tx, user, now)
		if err != nil {
			return err
		}
		var replacement RefreshToken
		if err := tx.Where("token_hash = ?", hashRefreshToken(issued.RefreshToken)).First(&replacement).Error; err != nil {
			return err
		}
		if err := tx.Model(&RefreshToken{}).Where("id = ?", row.ID).Update("replaced_by", replacement.ID).Error; err != nil {
			return err
		}
		pair = issued
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}
	if reuse {
		s.Audit(ctx, AuditEvent{Action: AuditRefreshReuse, TargetUserID: uintPtr(reuseUser)})
		return TokenPair{}, errRefreshReuse
	}
	return pair, nil
}

// RevokeRefresh invalidates one refresh token. Missing/already-revoked tokens
// succeed so logout is idempotent.
func (s *Service) RevokeRefresh(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errInvalidRefreshToken
	}
	now := s.now()
	hash := hashRefreshToken(raw)
	var row RefreshToken
	if err := s.db.WithContext(ctx).Where("token_hash = ? AND revoked_at IS NULL", hash).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.ID == 0 {
		return nil
	}
	result := s.db.WithContext(ctx).Model(&RefreshToken{}).
		Where("id = ? AND revoked_at IS NULL", row.ID).
		Update("revoked_at", now)
	if result.Error != nil {
		return result.Error
	}
	// RowsAffected: a concurrent logout of the same token records it once.
	if result.RowsAffected == 1 {
		s.Audit(ctx, AuditEvent{Action: AuditLogout, ActorUserID: uintPtr(row.UserID), TargetUserID: uintPtr(row.UserID)})
	}
	return nil
}

// ParseAccess validates an access JWT, from the Bearer header or the session
// cookie, and loads its user. It fails when the token's session has ended:
// its refresh token row was revoked or has expired.
func (s *Service) ParseAccess(ctx context.Context, token string) (User, error) {
	user, _, err := s.parseAccess(ctx, token)
	return user, err
}

// parseAccess is ParseAccess plus the id of the refresh token row the access
// token is bound to, which identifies the request's session. The token stops
// authenticating when its session ends: revoked or expired.
func (s *Service) parseAccess(ctx context.Context, token string) (User, uint, error) {
	claims, err := parseAccessToken(s.secret, token, s.now())
	if err != nil {
		return User{}, 0, err
	}
	id, err := userIDFromSubject(claims.Subject)
	if err != nil {
		return User{}, 0, err
	}
	var user User
	if err := s.db.WithContext(ctx).First(&user, id).Error; err != nil {
		return User{}, 0, errInvalidAccessToken
	}
	var row RefreshToken
	if err := s.db.WithContext(ctx).Where("id = ?", claims.RefreshID).First(&row).Error; err != nil {
		return User{}, 0, errInvalidAccessToken
	}
	// One definition of an active session, shared with the sessions API: an
	// unrevoked refresh token row that has not expired. The refresh TTL is
	// at least the access TTL (config validation and NewService), so a token
	// issued now never outlives its row. The expiry check stays for the ones
	// that can: a token issued before that rule, or before the operator
	// raised the refresh TTL, and one whose row's expiry has changed. It
	// would authenticate while its session is gone from the list and cannot
	// be revoked.
	if row.UserID != user.ID || row.RevokedAt != nil || !row.ExpiresAt.After(s.now()) {
		return User{}, 0, errInvalidAccessToken
	}
	return user, row.ID, nil
}

// lockUserTx loads the user and locks its row for the rest of tx. Token
// rotation (its reuse cascade included) and the three session revocations
// (one, the others, all) take this lock first, so they run one at a time per
// user. Login (IssueTokens) and logout (RevokeRefresh) do not take it, so
// they are not serialized with those (docs/auth.md documents it for logout).
// Without the lock, on PostgreSQL a revocation that waits on a row a
// concurrent rotation holds re-checks only that row and never sees the
// replacement the rotation inserts, so the session survives. SQLite has no
// row locks (the dialect drops FOR UPDATE); its single writer already
// serializes these transactions.
//
// Lock order: user before token, always. Whatever takes this lock must take
// it before it reads-to-modify or writes any refresh_tokens row, because the
// other path may already hold the user and be about to update a token. Taking
// the token first would deadlock against it.
//
// PostgreSQL gets FOR NO KEY UPDATE rather than FOR UPDATE. Inserting a row
// that references the user (a group membership, or any application table
// with a foreign key to users) takes FOR KEY SHARE on the user's row, which
// FOR UPDATE blocks for the whole transaction, so a login-time write would
// wait behind every rotation of that user. FOR NO KEY UPDATE conflicts with
// itself, which is all the serialization needs, but not with FOR KEY SHARE.
// MySQL has no such lock mode, so it keeps FOR UPDATE.
func lockUserTx(tx *gorm.DB, userID uint) (User, error) {
	strength := clause.LockingStrengthUpdate
	if tx.Name() == "postgres" { // the dialector's name
		strength = "NO KEY UPDATE"
	}
	var user User
	err := tx.Clauses(clause.Locking{Strength: strength}).First(&user, userID).Error
	return user, err
}

// revokeAllTx revokes every active refresh token of the user. tx must hold
// lockUserTx.
func revokeAllTx(tx *gorm.DB, userID uint, now time.Time) error {
	return tx.Model(&RefreshToken{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", now).Error
}

func (s *Service) compareDummy(password string) {
	s.dummyOnce.Do(func() {
		hash, err := s.hasher.Hash("timing-dummy-password")
		if err != nil {
			return
		}
		s.dummyHash = hash
	})
	if s.dummyHash == "" {
		return
	}
	_ = s.hasher.Compare(s.dummyHash, password)
}

// TokenPair is the login/refresh success payload (inside D10 data).
type TokenPair struct {
	AccessToken  string `json:"access_token" example:"eyJhbGciOiJIUzI1NiJ9..." doc:"Short-lived Bearer access JWT. Hold in memory only."`
	RefreshToken string `json:"refresh_token" example:"aa11..." doc:"Opaque rotating refresh token. Hold in memory only; never web storage."`
	TokenType    string `json:"token_type" example:"Bearer" doc:"Token type; always Bearer"`
	ExpiresIn    int    `json:"expires_in" example:"900" doc:"Access token lifetime in seconds"`
}

// PublicUser is the safe user representation returned by register and /me.
type PublicUser struct {
	ID            uint   `json:"id" example:"1" doc:"User identifier"`
	Email         string `json:"email" example:"ada@example.com" format:"email" doc:"Normalized email"`
	EmailVerified bool   `json:"email_verified" example:"false" doc:"True once the user has confirmed they own email (POST /auth/verify-email)"`
}

func toPublicUser(user User) PublicUser {
	return PublicUser{ID: user.ID, Email: user.Email, EmailVerified: user.EmailVerified()}
}
