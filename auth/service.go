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
	db      *gorm.DB
	cfg     config.AuthConfig
	secret  []byte
	hasher  Hasher
	clock   Clock
	mailer  Mailer
	logger  *zap.Logger
	auditor Auditor
	// background runs the work the routes accept and finish after
	// answering (background.go); Close drains it.
	background *backgroundRunner
	// throttle counts the credential endpoints' attempts (throttle.go).
	throttle    *throttler
	dummyOnce   sync.Once
	dummyHash   string
	rotateMu    sync.Mutex
	rotateCalls map[string]*rotateCall
	// rotateTimeout bounds a shared rotation (zero: defaultRotateTimeout).
	rotateTimeout time.Duration
	// onRotateJoin, when set (tests only), runs when a caller joins another
	// caller's in-flight rotation, just before it waits for the result.
	onRotateJoin func()
	// onLoginAuthenticated, when set (tests only), runs in Login between the
	// password check and the session.
	onLoginAuthenticated func()
	// onPasswordChangeVerified, when set (tests only), runs in
	// ChangePassword between the current-password check and the transaction.
	onPasswordChangeVerified func()
}

type rotateCall struct {
	wg   sync.WaitGroup
	pair TokenPair
	user User
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
	s.background = newBackgroundRunner()
	s.throttle = &throttler{
		policy:   DefaultThrottlePolicy(),
		disabled: cfg.Auth.ThrottleDisabled,
		now:      s.now,
		logger:   func() *zap.Logger { return s.logger },
	}
	for _, opt := range opts {
		opt(s)
	}
	if !s.throttle.disabled {
		// Off the request path: whether the throttles' store keeps counts.
		s.throttle.startProbe()
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
// Duplicate emails return errEmailTaken; an empty email
// ErrInvalidCredentials; any other email that is not a bare address
// ErrInvalidEmail, and a password the policy refuses a PasswordPolicyError
// (CheckNewAccount; #461).
func (s *Service) Register(ctx context.Context, email, password string) (User, error) {
	// Every registration counts (RegisterPerAddress), before the hash.
	if _, err := s.throttle.reserve(ctx, throttleRule{name: "RegisterPerAddress", limit: s.throttle.policy.RegisterPerAddress, subject: requestAddress(ctx)}); err != nil {
		return User{}, err
	}
	user, err := s.createUser(ctx, email, password, false)
	if err != nil {
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditRegistered, ActorUserID: uintPtr(user.ID), TargetUserID: uintPtr(user.ID)})
	s.sendVerificationOnRegister(ctx, user)
	return user, nil
}

// CreateSuperuser creates a user with IsSuperuser set, for gombit
// createsuperuser (M4-6). It shares Register's hasher, uniqueness path and
// rules: an empty email is ErrInvalidCredentials, any other one that is not
// a bare address ErrInvalidEmail, a password the policy refuses a
// PasswordPolicyError (CheckNewAccount), a duplicate email errEmailTaken.
// The operator who creates it vouches for the address, so its email starts
// verified, and no message is sent.
func (s *Service) CreateSuperuser(ctx context.Context, email, password string) (User, error) {
	user, err := s.createUser(ctx, email, password, true)
	if err != nil {
		return User{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditSuperuserCreated, TargetUserID: uintPtr(user.ID)})
	return user, nil
}

// CheckNewAccount reports whether email and password meet the account rules
// Register and CreateSuperuser apply, without touching the database (gombit
// createsuperuser checks before it opens one); those also refuse a duplicate
// email and a throttled address. Its verdict matches theirs, but a blank
// email, which it refuses as ErrInvalidEmail, is ErrInvalidCredentials from
// them. Two rules, for two reasons (#461):
//
//   - the email, normalized, must be a bare address of at most 255
//     characters (ErrInvalidEmail): what POST /auth/login accepts, so the
//     account can sign in;
//   - the password must meet the policy a user's own password meets on
//     register, change and reset, at least 8 characters and at most 72
//     bytes (a PasswordPolicyError on "password"). Login itself accepts any
//     non-empty password; this is the policy, not a sign-in requirement.
func CheckNewAccount(email, password string) error {
	if !validEmail(normalizeEmail(email)) {
		return errInvalidEmail
	}
	return checkNewPassword(password, "password")
}

func (s *Service) createUser(ctx context.Context, email, password string, superuser bool) (User, error) {
	email = normalizeEmail(email)
	if email == "" {
		return User{}, errInvalidCredentials
	}
	if err := CheckNewAccount(email, password); err != nil {
		return User{}, err
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

// Authenticate verifies email and password. It fails with
// ErrInvalidCredentials for an unknown email or a wrong password (a 401),
// ErrAccountDisabled for the right password of a disabled account (a 403),
// and a *ThrottleError (errors.Is ErrTooManyAttempts, a 429) past a login
// throttle; any other error is a server failure (the user lookup's
// database error), which a caller must not answer as wrong credentials
// (#460).
func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	email = normalizeEmail(email)
	// Throttled before the lookup and bcrypt, by the email as typed: the
	// same for an account and an address that is none. A success does not
	// count, and forgets this address's failures for the email.
	pair, rules := s.loginRules(ctx, email)
	res, err := s.throttle.reserve(ctx, rules...)
	if err != nil {
		return User{}, err
	}
	user, err := findUserByEmail(s.db.WithContext(ctx), email)
	if err != nil {
		// The database failed, not the credentials: a 500, so a blip does
		// not tell the user their password is wrong, and the attempt gives
		// its throttle places back, so an outage spends nobody's login
		// budget (issue #460).
		res.refund()
		return User{}, err
	}
	if user.ID == 0 {
		s.compareDummy(password)
		// No target: the attempted address is not recorded.
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, Details: details("reason", "unknown_account")})
		return User{}, errInvalidCredentials
	}
	if err := s.hasher.Compare(user.PasswordHash, password); err != nil {
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "wrong_password")})
		return User{}, errInvalidCredentials
	}
	// Said only to whoever knows the password: before it, a disabled
	// account answers like any other. It still counts as a failure.
	if user.Disabled() {
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "account_disabled")})
		return User{}, errAccountDisabled
	}
	res.refundExcept(pair.name)
	s.throttle.clear(ctx, pair)
	s.throttle.markKnown(ctx, pair)
	return user, nil
}

// Login verifies email and password and starts a session, as POST
// /auth/login does. The session is created under the user's row lock, and
// only if the password hash it verified is still the user's: a password
// reset (or any change of the hash) that commits between the check and the
// session either finds the session and revokes it, or makes this login fail
// as invalid credentials. The old password never outlives its change.
func (s *Service) Login(ctx context.Context, email, password string) (User, TokenPair, error) {
	user, err := s.Authenticate(ctx, email, password)
	if err != nil {
		return User{}, TokenPair{}, err
	}
	if s.onLoginAuthenticated != nil {
		s.onLoginAuthenticated()
	}
	return s.startSession(ctx, user)
}

// IssueTokens returns a new access JWT and a rotating refresh token for
// user: it starts a session, so it records the login as succeeded
// (Authenticate records the failures). user is a snapshot, as Authenticate
// returned it or as loaded from the database: its PasswordHash must still
// be the user's when the session starts, and the user must still exist, or
// IssueTokens fails as invalid credentials (ErrInvalidCredentials); the
// account must not be disabled (ErrAccountDisabled); any other error is a
// server failure. So Authenticate followed by IssueTokens is as safe as
// Login: a password reset or a disable that commits between the two leaves
// the snapshot unable to start a session.
func (s *Service) IssueTokens(ctx context.Context, user User) (TokenPair, error) {
	_, pair, err := s.startSession(ctx, user)
	return pair, err
}

// startSession issues a session for user under the user's row lock, the lock
// a password reset and the session revocations take, so it is ordered
// against them, and only if the stored password hash is still the one in
// user's snapshot.
func (s *Service) startSession(ctx context.Context, user User) (User, TokenPair, error) {
	var pair TokenPair
	var locked User
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		locked, err = lockUserTx(tx, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errLoginAccountGone
		}
		if err != nil {
			return err
		}
		if locked.Disabled() {
			return errAccountDisabled
		}
		if locked.PasswordHash != user.PasswordHash {
			return errLoginPasswordChanged
		}
		pair, err = s.issueTokens(tx, locked, s.now())
		return err
	})
	if errors.Is(err, errLoginPasswordChanged) {
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "password_changed")})
		return User{}, TokenPair{}, errInvalidCredentials
	}
	if errors.Is(err, errLoginAccountGone) {
		// Deleted between the password check and the session: to the
		// client, an account that no longer exists, a 401 like an unknown
		// email, not a server failure (#460).
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "account_deleted")})
		return User{}, TokenPair{}, errInvalidCredentials
	}
	if errors.Is(err, errAccountDisabled) {
		// Disabled after the password was checked: recorded as Authenticate
		// records a disabled account's login.
		s.Audit(ctx, AuditEvent{Action: AuditLoginFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "account_disabled")})
		return User{}, TokenPair{}, err
	}
	if err != nil {
		return User{}, TokenPair{}, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditLoginSucceeded, ActorUserID: uintPtr(locked.ID), TargetUserID: uintPtr(locked.ID)})
	return locked, pair, nil
}

// errLoginPasswordChanged is a login whose verified password was replaced
// before its session started (Login answers it as invalid credentials).
var errLoginPasswordChanged = errors.New("auth: the password changed during the login")

// errLoginAccountGone is a login whose user was deleted before its session
// started (Login and IssueTokens answer it as invalid credentials).
var errLoginAccountGone = errors.New("auth: the account was deleted during the login")

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
// new pair. Reuse of an already-rotated token before it expires revokes the
// user's remaining tokens, unless it comes within the configured
// RefreshReuseGrace of the rotation while the successor is still live, a lost
// race that is just invalid (and audited); a token revoked by logout or
// session revocation, and any expired token, is just invalid. Concurrent
// callers with the same still-valid token share one rotation so a lost race
// cannot family-revoke the winner's new session. That rotation runs detached
// from every caller's cancellation and deadline (s.detach, cancelled only by
// its own timeout, rotateTimeout, which bounds its transaction, or by Close),
// and each caller gets its one result: a caller whose client went away cannot
// fail the others with its cancellation (#462). So RotateRefresh does not
// return early when ctx is done: every caller, the first included, waits for
// the shared rotation (its transaction for at most rotateTimeout, then the
// audit record of a reuse for at most auditTimeout) and gets its result.
func (s *Service) RotateRefresh(ctx context.Context, raw string) (TokenPair, error) {
	pair, _, err := s.rotateRefresh(ctx, raw)
	return pair, err
}

// rotateRefresh is RotateRefresh, plus the session's user as rotateRefreshOnce
// loaded it inside the rotation's transaction. refreshCookie uses it instead
// of a second, post-commit lookup: a failure loading the user would otherwise
// surface after the rotation already committed, leaving the browser's old
// refresh cookie rotated with no way to tell it (#606).
func (s *Service) rotateRefresh(ctx context.Context, raw string) (TokenPair, User, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return TokenPair{}, User{}, errInvalidRefreshToken
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
		return call.pair, call.user, call.err
	}
	call := &rotateCall{}
	call.wg.Add(1)
	s.rotateCalls[hash] = call
	s.rotateMu.Unlock()

	// The rotation is shared: every caller that joins it gets its result,
	// so it must not end with this caller's request. It runs under the
	// request's values (the audit trail's request id and address) without
	// its cancellation (s.detach), bounded by rotateTimeout and cancelled
	// by Close. A client that goes away, or a request deadline that passes,
	// mid-rotation no longer fails the followers with that cancellation
	// (#462). The trade: the rotation commits even though that first
	// client is gone and never receives the new pair, so its next refresh
	// with the old token finds it rotated: within RefreshReuseGrace, while
	// the new session is live, a lost race (errRotationRaced, a 401 that
	// revokes nothing); later, reuse, which revokes every session. And
	// the rotation's own cancellation still reaches the driver: a
	// rotateTimeout or a Close that lands after COMMIT was sent can report
	// failure for a rotation the server applied.
	rctx, cancel := s.detach(ctx, s.sharedRotateTimeout())
	pair, user, err := s.rotateRefreshOnce(rctx, raw, hash)
	cancel()
	call.pair, call.user, call.err = pair, user, err
	s.rotateMu.Lock()
	delete(s.rotateCalls, hash)
	s.rotateMu.Unlock()
	call.wg.Done()
	return pair, user, err
}

// defaultRotateTimeout bounds a shared rotation's transaction: one short
// transaction, which may wait behind the user's row lock (another rotation,
// a session revocation). A reuse's audit record, written after it, has its
// own auditTimeout.
const defaultRotateTimeout = 10 * time.Second

func (s *Service) sharedRotateTimeout() time.Duration {
	if s.rotateTimeout > 0 {
		return s.rotateTimeout
	}
	return defaultRotateTimeout
}

// rotateRefreshOnce runs one rotation in a transaction. Lock order: the
// user's row (lockUserTx) comes before any write to or locking read of a
// refresh_tokens row, the same order every revocation uses; the
// compare-and-swap and the re-reads below (hasSuccessorTx) must stay after
// it. The initial read of the token takes no lock. The rotation's
// instant is read once the lock is held: the lock can wait behind another
// rotation or a revocation, and a token that expired during that wait must
// not rotate, nor its successor's lifetime start before the wait.
func (s *Service) rotateRefreshOnce(ctx context.Context, raw, hash string) (TokenPair, User, error) {
	var pair TokenPair
	var user User
	var reuse, withinGrace bool
	var reuseUser uint
	var graceSession, graceSuccessor string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row RefreshToken
		if err := tx.Where("token_hash = ?", hash).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errInvalidRefreshToken
			}
			return err
		}
		var err error
		user, err = lockUserTx(tx, row.UserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errUserNotFound
			}
			// A deadlock, lock timeout or lost connection is a server
			// failure, not a dead credential: it must not read as a 401.
			return err
		}
		// A disabled account's tokens were revoked when it was disabled:
		// a dead credential, before any reuse check (its revoked tokens are
		// no evidence of theft).
		if user.Disabled() {
			return errInvalidRefreshToken
		}
		now := s.now()
		// Expiry comes before the reuse check: an expired token is a plain
		// 401, rotated or not (#458). A rotated row keeps its original
		// expires_at; past it the token cannot refresh anyone, and in cookie
		// mode the browser has already dropped it with the cookie's Max-Age,
		// so a stale copy (a lost rotation response, a restored backup) must
		// not end every live session. The cost: theft is detected only while
		// the rotated token is unexpired. If a thief rotated it first and the
		// legitimate client comes back only after it expired, the thief's
		// session survives this 401; the sessions list is how the user finds
		// and revokes it.
		if !row.ExpiresAt.After(now) {
			// An expired token that has a successor was rotated, perhaps by
			// another tab of the same browser whose new session is live: it
			// is errRotationRaced, still a 401, so that refreshCookie does
			// not clear that session's cookies. Only a rotation leaves a
			// successor; a token revoked by logout, a session revocation or
			// the reuse cascade has none, and its 401 clears them. If the
			// successor was revoked since, the cookies stay this time, and a
			// refresh with the successor's cookie clears them.
			rotated := row.ReplacedBy != nil
			if row.RevokedAt == nil {
				// The read above took no lock, and a rotation of this token
				// may have committed while this one waited for the user's
				// lock: re-read the row, with a locking read
				// (hasSuccessorTx). SQLite's dialect drops the clause. Within
				// one process this race cannot happen: the process joins
				// concurrent rotations of a token into one. Several
				// processes sharing one database file are not covered: in
				// WAL mode another process's rotation can commit while this
				// transaction reads from its older snapshot, and this
				// refresh then finds no successor and clears the cookies.
				if rotated, err = hasSuccessorTx(tx, row.ID); err != nil {
					// A server failure, like the lock's: not a 401.
					return err
				}
			}
			if rotated {
				return errRotationRaced
			}
			return errInvalidRefreshToken
		}
		if row.RevokedAt != nil {
			if row.ReplacedBy == nil {
				// Revoked by logout or session revocation, not by rotation:
				// presenting it again is a dead credential, not evidence of
				// theft, so it must not end the user's other sessions.
				return errInvalidRefreshToken
			}
			// Rotated moments ago, and its successor is still the live head:
			// another tab of the same browser sent this token before the
			// winner's response came back, its refresh on the wire for a
			// round trip, not merely overlapping inside this process (which
			// the single-flight above joins). A lost race, errRotationRaced:
			// a 401 that issues and revokes nothing, so the winner's new
			// session survives and, in cookie mode, keeps the cookies it was
			// just sent (issue #459). A successor revoked since (a logout)
			// leaves nothing to race: a plain invalid token. One rotated
			// again has moved past any sibling tab: reuse. Either way short
			// of the cascade is audited, since it is also all a thief who
			// rotated first leaves when the legitimate client comes back.
			if grace := s.cfg.RefreshReuseGrace; grace > 0 && row.RevokedAt.After(now.Add(-grace)) {
				successor, err := lockRefreshRowTx(tx, *row.ReplacedBy)
				if err != nil {
					return err
				}
				switch {
				case successor.ReplacedBy != nil:
					// Rotated again: fall through to the cascade.
				case successor.RevokedAt == nil && successor.ExpiresAt.After(now):
					withinGrace, reuseUser = true, row.UserID
					graceSession, graceSuccessor = s.sessionID(successor.ID), "live"
					return errRotationRaced
				default:
					withinGrace, reuseUser = true, row.UserID
					graceSession, graceSuccessor = s.sessionID(successor.ID), "ended"
					return errInvalidRefreshToken
				}
			}
			// Rotated, and presented again later, before it expired: reuse.
			if err := revokeAllTx(tx, row.UserID, now); err != nil {
				return err
			}
			reuse, reuseUser = true, row.UserID
			return nil
		}

		result := tx.Model(&RefreshToken{}).
			Where("id = ? AND revoked_at IS NULL", row.ID).
			Updates(map[string]any{"revoked_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			// Lost the compare-and-swap: the token was revoked after the
			// unlocked read above. Either a rotation of the same token on
			// another instance won and gave it a successor, or something
			// revoked it with none: a logout or a session revocation that
			// committed while this rotation waited for the user's lock, or
			// the reuse cascade. None of them is
			// reuse-as-theft; do not revoke anything. The re-read tells them
			// apart, as for an expired token above. A successor means a
			// rotation won: errRotationRaced, and refreshCookie leaves the
			// cookies the winner's response sets. None means the token was
			// revoked: a plain invalid token, whose 401 clears the dead
			// cookies.
			rotated, err := hasSuccessorTx(tx, row.ID)
			if err != nil {
				// A server failure, like the lock's: not a 401.
				return err
			}
			if rotated {
				return errRotationRaced
			}
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
	if withinGrace {
		// The successor is the session to look at: a sibling tab's, or a
		// thief's who rotated first. "live" is a session that may be
		// either; "ended" was signed out before this request came.
		s.Audit(ctx, AuditEvent{
			Action:       AuditRefreshReuseWithinGrace,
			TargetUserID: uintPtr(reuseUser),
			TargetType:   "session",
			TargetID:     graceSession,
			Details:      details("successor", graceSuccessor),
		})
	}
	if err != nil {
		return TokenPair{}, User{}, err
	}
	if reuse {
		s.Audit(ctx, AuditEvent{Action: AuditRefreshReuse, TargetUserID: uintPtr(reuseUser)})
		return TokenPair{}, User{}, errRefreshReuse
	}
	return pair, user, nil
}

// hasSuccessorTx re-reads the refresh token row id in tx, which holds the
// user's lock (lockUserTx), and reports whether a rotation has given it a
// successor. It is a locking read: on MySQL (REPEATABLE READ) a plain read
// sees the snapshot this transaction's first read took, from before a
// rotation that committed while it waited for the user's lock, and an UPDATE
// that matched no row does not move that snapshot; a locking read sees the
// latest committed row. PostgreSQL (READ COMMITTED) would see it either way.
func hasSuccessorTx(tx *gorm.DB, id uint) (bool, error) {
	var current RefreshToken
	if err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthShare}).
		First(&current, id).Error; err != nil {
		return false, err
	}
	return current.ReplacedBy != nil, nil
}

// lockRefreshRowTx reads refresh token row id in tx, which holds the user's
// lock, with a locking read for the same reason as hasSuccessorTx: the latest
// committed row, not this transaction's snapshot.
func lockRefreshRowTx(tx *gorm.DB, id uint) (RefreshToken, error) {
	var row RefreshToken
	err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthShare}).First(&row, id).Error
	return row, err
}

// RevokeRefresh ends the session of a refresh token: the token itself, or,
// when the token has rotated (a refresh in flight as the user signs out, or
// a client still holding the previous token), the session's current head,
// fewer than maxChainHops (1000) rotations on (#466): any unexpired token
// of the session's chain fewer than that many rotations behind the head ends
// it. One maxChainHops or more behind, or a chain that cannot be followed
// (errChainUnresolved), fails with a 500 rather than succeed having ended
// nothing; revoking the session (DELETE /auth/sessions/{id}, or
// ?scope=all) ends it then. A token unknown, past its
// own expiry, or of a session already ended revokes nothing and succeeds,
// alike, so logout is idempotent and its answer says nothing about a token;
// a token past its expiry is no credential, as for refresh (#458). It takes the user's lock (lockUserTx) like rotation does, so a
// logout and a refresh of the same token run one after the other: whichever
// comes second sees the other's result, and the session ends either way.
func (s *Service) RevokeRefresh(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errInvalidRefreshToken
	}
	hash := hashRefreshToken(raw)
	// This lookup must stay outside the transaction below. sessionChain
	// walks with plain reads, and on MySQL (REPEATABLE READ) a transaction's
	// plain reads all see the snapshot its first plain read takes: inside,
	// this read would take it before the user's lock, and the walk would
	// miss a rotation that committed during the wait, revoking an already
	// rotated row and ending nothing. With the lock (a locking read, which
	// takes no snapshot) first, the walk's first read takes the snapshot
	// after it.
	var row RefreshToken
	if err := s.db.WithContext(ctx).Where("token_hash = ?", hash).Limit(1).Find(&row).Error; err != nil {
		return err
	}
	if row.ID == 0 {
		return nil
	}
	var ended *RefreshToken
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, row.UserID); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // the user is gone, and its sessions with it
			}
			return err
		}
		now := s.now()
		// The token's own expiry, as rotation reads it: past it, a token
		// refreshes nothing and so ends nothing either.
		if !row.ExpiresAt.After(now) {
			return nil
		}
		_, head, err := sessionChain(tx, row.UserID, row.ID)
		if err != nil || head == nil || head.RevokedAt != nil || !head.ExpiresAt.After(now) {
			return err
		}
		result := tx.Model(&RefreshToken{}).
			Where("id = ? AND revoked_at IS NULL", head.ID).
			Update("revoked_at", now)
		if result.RowsAffected == 1 {
			ended = head
		}
		return result.Error
	})
	if err != nil {
		return err
	}
	// Under the lock a concurrent logout of the same session waits and then
	// finds it ended: it is recorded once. The event names the session it
	// ended, and says when a rotated token ended it: an old copy of the
	// session's token, which may be a sibling tab's or a thief's.
	if ended != nil {
		event := AuditEvent{
			Action:       AuditLogout,
			ActorUserID:  uintPtr(row.UserID),
			TargetUserID: uintPtr(row.UserID),
			TargetType:   "session",
			TargetID:     s.sessionID(ended.ID),
		}
		if ended.ID != row.ID {
			event.Details = details("via", "rotated")
		}
		s.Audit(ctx, event)
	}
	return nil
}

// ParseAccess validates an access JWT, from the Bearer header or the session
// cookie, and loads its user. It fails with ErrInvalidAccessToken when the
// token is invalid or its session has ended (its refresh token row is gone,
// revoked or expired); any other error is the database's, loading the user
// or the session, a server failure a gate must not answer as a 401 (#460).
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
	// Only a missing row is a dead token. Any other failure is the
	// database's: returned as is, a 500 from the gates, so a blip does not
	// read as a dead session and sign every client out (issue #460).
	var user User
	if err := s.db.WithContext(ctx).First(&user, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, 0, errInvalidAccessToken
		}
		return User{}, 0, err
	}
	// Its sessions were revoked when it was disabled; refused here too, so
	// no session that outlived that (a race, a restore) authenticates it.
	if user.Disabled() {
		return User{}, 0, errInvalidAccessToken
	}
	var row RefreshToken
	if err := s.db.WithContext(ctx).Where("id = ?", claims.RefreshID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, 0, errInvalidAccessToken
		}
		return User{}, 0, err
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
// user, and so do logout (RevokeRefresh, which follows a rotated token to
// its session's head, #466) and login's session start (IssueTokens,
// startSession): every writer of refresh_tokens. Only login's password check
// (bcrypt, slow) runs before the lock (docs/auth.md).
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
