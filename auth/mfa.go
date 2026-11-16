package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- TOTP (RFC 6238) is HMAC-SHA1 by its spec and every authenticator app.
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Multi-factor authentication (AUTH-9, #339): a user may enroll a TOTP
// authenticator (RFC 6238: HMAC-SHA1, 30-second steps, 6 digits), and from
// then on a login needs, besides the password, a code from it or one of the
// account's single-use recovery codes (mfa_code). The TOTP secret is stored
// encrypted (AES-256-GCM) and the recovery codes hashed (HMAC-SHA256), both
// under keys derived from AuthConfig.MFAKey (or JWTSecret when it is unset).
// A code's time step is accepted once: a code seen is never accepted again.

// TOTPFactor is a user's TOTP authenticator: pending (ConfirmedAt nil) from
// the enrollment until a code from it is confirmed, then enabled.
type TOTPFactor struct {
	ID     uint `gorm:"primaryKey"`
	UserID uint `gorm:"uniqueIndex;not null"`
	// Secret is the TOTP key, encrypted (never stored or logged in clear).
	Secret string `gorm:"size:255;not null"`
	// ConfirmedAt is when the enrollment was confirmed; nil while pending.
	ConfirmedAt *time.Time
	// LastStep is the last time step whose code was accepted: no code of
	// it, or of an earlier one, is accepted again.
	LastStep  int64 `gorm:"not null;default:0"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName is the factors' table.
func (TOTPFactor) TableName() string { return "auth_totp_factors" }

// RecoveryCode is one of a user's single-use MFA recovery codes, hashed.
type RecoveryCode struct {
	ID       uint   `gorm:"primaryKey"`
	UserID   uint   `gorm:"index;not null"`
	CodeHash string `gorm:"uniqueIndex;size:64;not null"`
	// UsedAt is when the code was used; a used code is refused.
	UsedAt    *time.Time
	CreatedAt time.Time
}

// TableName is the recovery codes' table.
func (RecoveryCode) TableName() string { return "auth_recovery_codes" }

const (
	totpPeriod        = 30
	totpDigits        = 6
	totpSkew          = 1 // steps accepted either side of now: clock drift
	totpSecretBytes   = 20
	recoveryCodeCount = 10
	recoveryCodeChars = 10
)

var (
	errMFARequired          = errors.New("auth: a multi-factor code is required")
	errInvalidMFACode       = errors.New("auth: invalid multi-factor code")
	errMFAAlreadyEnabled    = errors.New("auth: multi-factor authentication is already enabled")
	errMFANotEnabled        = errors.New("auth: multi-factor authentication is not enabled")
	errNoPendingEnrollment  = errors.New("auth: no multi-factor enrollment is in progress")
	errMFASecretUnreadable  = errors.New("auth: the TOTP secret cannot be decrypted (was the MFA key, or the JWT secret it derives from, changed?)")
	errCannotResetOwnMFA    = errors.New("auth: an account resets its own multi-factor authentication by disabling it")
)

// recoveryAlphabet is the recovery codes' letters: no 0/o, 1/l/i to confuse.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// totpSecretEncoding is how an authenticator app takes a secret: RFC 4648
// base32, unpadded.
var totpSecretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrMFARequired is a login of an account with MFA enabled that gave no
// code: the password was right (it is told only then), a code is needed.
var ErrMFARequired = errMFARequired

// ErrInvalidMFACode is a code that is not the authenticator's current one
// (or was used already), nor an unused recovery code.
var ErrInvalidMFACode = errInvalidMFACode

// ErrMFAAlreadyEnabled is an enrollment of an account with MFA enabled.
var ErrMFAAlreadyEnabled = errMFAAlreadyEnabled

// ErrMFANotEnabled is an operation on the MFA of an account without it.
var ErrMFANotEnabled = errMFANotEnabled

// ErrNoPendingEnrollment is a confirmation with no enrollment begun.
var ErrNoPendingEnrollment = errNoPendingEnrollment

// TOTPEnrollment is what an authenticator app needs: the secret (base32,
// for typing it in) and the otpauth:// URI (for a QR code).
type TOTPEnrollment struct {
	Secret string
	URI    string
}

// MFAStatus is a user's MFA state.
type MFAStatus struct {
	Enabled           bool
	PendingEnrollment bool
	// RecoveryCodesLeft is how many unused recovery codes remain.
	RecoveryCodesLeft int
}

// mfaKey derives the key for purpose ("totp", "recovery") from MFAKey, or
// JWTSecret when it is unset.
func (s *Service) mfaKey(purpose string) []byte {
	material := s.cfg.MFAKey
	if material == "" {
		material = s.cfg.JWTSecret
	}
	key, err := hkdf.Key(sha256.New, []byte(material), nil, "gombit auth mfa "+purpose+" v1", 32)
	if err != nil {
		panic(err) // only for a key length sha256 cannot give
	}
	return key
}

// sealTOTPSecret encrypts secret for userID: the user's id is the
// additional data, so a row's secret moved to another user's does not open.
func (s *Service) sealTOTPSecret(userID uint, secret []byte) (string, error) {
	gcm, err := newGCM(s.mfaKey("totp"))
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, secret, userAAD(userID))
	return "v1." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *Service) openTOTPSecret(userID uint, stored string) ([]byte, error) {
	raw, ok := strings.CutPrefix(stored, "v1.")
	if !ok {
		return nil, errMFASecretUnreadable
	}
	sealed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errMFASecretUnreadable
	}
	gcm, err := newGCM(s.mfaKey("totp"))
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errMFASecretUnreadable
	}
	secret, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], userAAD(userID))
	if err != nil {
		return nil, errMFASecretUnreadable
	}
	return secret, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func userAAD(userID uint) []byte {
	return binary.BigEndian.AppendUint64([]byte("gombit-totp-user:"), uint64(userID))
}

// totpCode is the RFC 4226 code of key at counter step.
func totpCode(key []byte, step int64) string {
	mac := hmac.New(sha1.New, key)
	_ = binary.Write(mac, binary.BigEndian, uint64(step)) // #nosec G115 -- a step is never negative: Unix time / 30.
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1_000_000)
}

func totpStep(now time.Time) int64 { return now.Unix() / totpPeriod }

// normalizeMFACode strips what people type around a code: spaces and dashes,
// and case.
func normalizeMFACode(code string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(code)))
}

func isTOTPShaped(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (s *Service) hashRecoveryCode(code string) string {
	mac := hmac.New(sha256.New, s.mfaKey("recovery"))
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

// newRecoveryCodes makes recoveryCodeCount codes: the ones to show (grouped
// as xxxxx-xxxxx) and their hashes.
func (s *Service) newRecoveryCodes() (shown, hashes []string, err error) {
	// Rejection sampling: a byte is used only below the largest multiple of
	// the alphabet's size, so every letter is as likely as any other.
	limit := byte(256 / len(recoveryAlphabet) * len(recoveryAlphabet))
	one := make([]byte, 1)
	for range recoveryCodeCount {
		buf := make([]byte, 0, recoveryCodeChars)
		for len(buf) < recoveryCodeChars {
			if _, err := rand.Read(one); err != nil {
				return nil, nil, err
			}
			if one[0] < limit {
				buf = append(buf, recoveryAlphabet[int(one[0])%len(recoveryAlphabet)])
			}
		}
		code := string(buf)
		shown = append(shown, code[:5]+"-"+code[5:])
		hashes = append(hashes, s.hashRecoveryCode(code))
	}
	return shown, hashes, nil
}

func replaceRecoveryCodesTx(tx *gorm.DB, userID uint, hashes []string) error {
	if err := tx.Where("user_id = ?", userID).Delete(&RecoveryCode{}).Error; err != nil {
		return err
	}
	rows := make([]RecoveryCode, len(hashes))
	for i, h := range hashes {
		rows[i] = RecoveryCode{UserID: userID, CodeHash: h}
	}
	return tx.Create(&rows).Error
}

// confirmedFactor is userID's enabled TOTP factor, or ID 0 for none.
func confirmedFactor(db *gorm.DB, userID uint) (TOTPFactor, error) {
	var f TOTPFactor
	err := db.Where("user_id = ? AND confirmed_at IS NOT NULL", userID).Limit(1).Find(&f).Error
	return f, err
}

// MFAStatusOf is userID's MFA state.
func (s *Service) MFAStatusOf(ctx context.Context, userID uint) (MFAStatus, error) {
	db := s.db.WithContext(ctx)
	var f TOTPFactor
	if err := db.Where("user_id = ?", userID).Limit(1).Find(&f).Error; err != nil {
		return MFAStatus{}, err
	}
	status := MFAStatus{Enabled: f.ID != 0 && f.ConfirmedAt != nil, PendingEnrollment: f.ID != 0 && f.ConfirmedAt == nil}
	if status.Enabled {
		var left int64
		if err := db.Model(&RecoveryCode{}).Where("user_id = ? AND used_at IS NULL", userID).Count(&left).Error; err != nil {
			return MFAStatus{}, err
		}
		status.RecoveryCodesLeft = int(left)
	}
	return status, nil
}

// checkCurrentPassword checks current is userID's password, as a password
// or email change does (throttled by CurrentPasswordPerUser; a wrong one is
// ErrWrongCurrentPassword, audited with action).
func (s *Service) checkCurrentPassword(ctx context.Context, userID uint, current string, action AuditAction) (User, error) {
	var user User
	if err := s.db.WithContext(ctx).Where("id = ?", userID).Limit(1).Find(&user).Error; err != nil {
		return User{}, err
	}
	if user.ID == 0 {
		return User{}, errUserNotFound
	}
	res, err := s.throttle.reserve(ctx, s.currentPasswordRule(user.ID))
	if err != nil {
		return User{}, err
	}
	if err := s.hasher.Compare(user.PasswordHash, current); err != nil {
		s.Audit(ctx, AuditEvent{Action: action, TargetUserID: uintPtr(user.ID), Details: details("reason", "wrong_current_password")})
		return User{}, errWrongCurrentPassword
	}
	res.refund()
	return user, nil
}

// BeginTOTPEnrollment starts userID's TOTP enrollment, current being their
// password: it returns the new secret for their authenticator app, pending
// until ConfirmTOTPEnrollment gets a code from it (a new enrollment replaces
// one pending). It returns ErrMFAAlreadyEnabled when MFA is enabled (disable
// it first), and ErrWrongCurrentPassword (audited).
func (s *Service) BeginTOTPEnrollment(ctx context.Context, userID uint, current string) (TOTPEnrollment, error) {
	user, err := s.checkCurrentPassword(ctx, userID, current, AuditMFAFailed)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	secret := make([]byte, totpSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return TOTPEnrollment{}, err
	}
	sealed, err := s.sealTOTPSecret(user.ID, secret)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if locked.Disabled() {
			return errAccountDisabled
		}
		f, err := confirmedFactor(tx, user.ID)
		if err != nil {
			return err
		}
		if f.ID != 0 {
			return errMFAAlreadyEnabled
		}
		if err := tx.Where("user_id = ?", user.ID).Delete(&TOTPFactor{}).Error; err != nil {
			return err
		}
		return tx.Create(&TOTPFactor{UserID: user.ID, Secret: sealed}).Error
	})
	if err != nil {
		return TOTPEnrollment{}, err
	}
	encoded := totpSecretEncoding.EncodeToString(secret)
	label := s.mfaIssuer + ":" + user.Email
	query := url.Values{
		"secret":    {encoded},
		"issuer":    {s.mfaIssuer},
		"algorithm": {"SHA1"},
		"digits":    {strconv.Itoa(totpDigits)},
		"period":    {strconv.Itoa(totpPeriod)},
	}
	// PathEscape keeps '+', which an app form-decoding the label reads as a
	// space (ada+work@example.com): escaped too.
	escaped := strings.ReplaceAll(url.PathEscape(label), "+", "%2B")
	return TOTPEnrollment{Secret: encoded, URI: "otpauth://totp/" + escaped + "?" + query.Encode()}, nil
}

// ConfirmTOTPEnrollment enables MFA for userID with a code from the
// authenticator their pending enrollment set up, and returns the account's
// recovery codes, shown this once (only their hashes are kept). It returns
// ErrNoPendingEnrollment, ErrInvalidMFACode (throttled by MFAPerUser), and
// ErrMFAAlreadyEnabled.
func (s *Service) ConfirmTOTPEnrollment(ctx context.Context, userID uint, code string) ([]string, error) {
	res, err := s.throttle.reserve(ctx, s.mfaRule(userID))
	if err != nil {
		return nil, err
	}
	shown, hashes, err := s.newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	code = normalizeMFACode(code)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, userID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if locked.Disabled() {
			return errAccountDisabled
		}
		var f TOTPFactor
		if err := tx.Where("user_id = ?", userID).Limit(1).Find(&f).Error; err != nil {
			return err
		}
		if f.ID == 0 {
			return errNoPendingEnrollment
		}
		if f.ConfirmedAt != nil {
			return errMFAAlreadyEnabled
		}
		now := s.now()
		step, err := s.matchTOTP(userID, f, code, now)
		if err != nil {
			return err
		}
		if err := tx.Model(&TOTPFactor{}).Where("id = ?", f.ID).Updates(map[string]any{"confirmed_at": now, "last_step": step}).Error; err != nil {
			return err
		}
		return replaceRecoveryCodesTx(tx, userID, hashes)
	})
	if errors.Is(err, errInvalidMFACode) {
		s.Audit(ctx, AuditEvent{Action: AuditMFAFailed, TargetUserID: uintPtr(userID), Details: details("reason", "wrong_mfa_code")})
		return nil, err
	}
	res.refund()
	if err != nil {
		return nil, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditMFAEnabled, TargetUserID: uintPtr(userID)})
	return shown, nil
}

// matchTOTP is the step whose code code is, among the steps around now
// newer than f.LastStep, or ErrInvalidMFACode.
func (s *Service) matchTOTP(userID uint, f TOTPFactor, code string, now time.Time) (int64, error) {
	if !isTOTPShaped(code) {
		return 0, errInvalidMFACode
	}
	secret, err := s.openTOTPSecret(userID, f.Secret)
	if err != nil {
		return 0, err
	}
	current := totpStep(now)
	for step := current - totpSkew; step <= current+totpSkew; step++ {
		if step > f.LastStep && hmac.Equal([]byte(totpCode(secret, step)), []byte(code)) {
			return step, nil
		}
	}
	return 0, errInvalidMFACode
}

// useMFACodeTx accepts code for userID's enabled factor, in tx: a TOTP code
// of a step not yet used (the step recorded, conditionally, so two requests
// with one code cannot both pass), or an unused recovery code (used up). It
// returns whether a recovery code was used, or ErrInvalidMFACode.
func (s *Service) useMFACodeTx(tx *gorm.DB, userID uint, f TOTPFactor, code string, now time.Time) (bool, error) {
	code = normalizeMFACode(code)
	if isTOTPShaped(code) {
		step, err := s.matchTOTP(userID, f, code, now)
		if err != nil {
			return false, err
		}
		used := tx.Model(&TOTPFactor{}).Where("id = ? AND last_step < ?", f.ID, step).Update("last_step", step)
		if used.Error != nil {
			return false, used.Error
		}
		if used.RowsAffected != 1 {
			return false, errInvalidMFACode
		}
		return false, nil
	}
	if code == "" {
		return false, errInvalidMFACode
	}
	used := tx.Model(&RecoveryCode{}).
		Where("user_id = ? AND code_hash = ? AND used_at IS NULL", userID, s.hashRecoveryCode(code)).
		Update("used_at", now)
	if used.Error != nil {
		return false, used.Error
	}
	if used.RowsAffected != 1 {
		return false, errInvalidMFACode
	}
	return true, nil
}

// checkMFA is the second factor of a login of user: nothing when MFA is not
// enabled, else code must be accepted (ErrMFARequired for none,
// ErrInvalidMFACode for a wrong one, throttled by MFAPerUser).
func (s *Service) checkMFA(ctx context.Context, user User, code string) error {
	f, err := confirmedFactor(s.db.WithContext(ctx), user.ID)
	if err != nil {
		return err
	}
	if f.ID == 0 {
		return nil
	}
	if strings.TrimSpace(code) == "" {
		return errMFARequired
	}
	res, err := s.throttle.reserve(ctx, s.mfaRule(user.ID))
	if err != nil {
		return err
	}
	var recovery bool
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		recovery, err = s.useMFACodeTx(tx, user.ID, f, code, s.now())
		return err
	})
	if errors.Is(err, errMFASecretUnreadable) {
		s.warnMFAKey()
	}
	if err != nil {
		return err
	}
	res.refund()
	if recovery {
		s.Audit(ctx, AuditEvent{Action: AuditMFARecoveryCodeUsed, TargetUserID: uintPtr(user.ID)})
	}
	return nil
}

// RegenerateRecoveryCodes replaces userID's recovery codes (the old ones
// stop working) and returns the new ones, current being their password and
// code a code from their authenticator or a recovery code: new codes are a
// second factor, so a stolen session and password alone do not mint them.
// It returns ErrMFANotEnabled, ErrWrongCurrentPassword (audited) and
// ErrInvalidMFACode (throttled).
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID uint, current, code string) ([]string, error) {
	shown, hashes, err := s.newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	err = s.withSecondFactor(ctx, userID, current, code, func(tx *gorm.DB) error {
		return replaceRecoveryCodesTx(tx, userID, hashes)
	})
	if err != nil {
		return nil, err
	}
	s.Audit(ctx, AuditEvent{Action: AuditMFARecoveryCodesRegenerated, TargetUserID: uintPtr(userID)})
	return shown, nil
}

// withSecondFactor runs change on userID's MFA in a transaction under the
// user's lock, once current is their password and code is accepted for
// their enabled factor (a code used up as at a login; a recovery code used
// is audited). It returns ErrWrongCurrentPassword (audited, throttled),
// ErrMFANotEnabled, and ErrInvalidMFACode (audited, throttled).
func (s *Service) withSecondFactor(ctx context.Context, userID uint, current, code string, change func(tx *gorm.DB) error) error {
	user, err := s.checkCurrentPassword(ctx, userID, current, AuditMFAFailed)
	if err != nil {
		return err
	}
	res, err := s.throttle.reserve(ctx, s.mfaRule(user.ID))
	if err != nil {
		return err
	}
	var recovery bool
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockUserTx(tx, user.ID); err != nil {
			return err
		}
		f, err := confirmedFactor(tx, user.ID)
		if err != nil {
			return err
		}
		if f.ID == 0 {
			return errMFANotEnabled
		}
		if recovery, err = s.useMFACodeTx(tx, user.ID, f, code, s.now()); err != nil {
			return err
		}
		return change(tx)
	})
	if errors.Is(err, errInvalidMFACode) {
		s.Audit(ctx, AuditEvent{Action: AuditMFAFailed, TargetUserID: uintPtr(user.ID), Details: details("reason", "wrong_mfa_code")})
		return err
	}
	if errors.Is(err, errMFASecretUnreadable) {
		s.warnMFAKey()
	}
	res.refund()
	if err != nil {
		return err
	}
	if recovery {
		s.Audit(ctx, AuditEvent{Action: AuditMFARecoveryCodeUsed, TargetUserID: uintPtr(user.ID)})
	}
	return nil
}

// warnMFAKey says, at most once a minute, that a TOTP secret could not be
// decrypted: the MFA key (or the JWT secret it derives from) changed, and
// the logins of the accounts enrolled before answer 500 until their MFA is
// reset.
func (s *Service) warnMFAKey() {
	now := time.Now().Unix()
	last := s.mfaKeyWarned.Load()
	if now-last < 60 || !s.mfaKeyWarned.CompareAndSwap(last, now) {
		return
	}
	s.logger.Warn("auth: a TOTP secret cannot be decrypted: GOMBIT_AUTH_MFA_KEY (or GOMBIT_JWT_SECRET, which it derives from when unset) changed since the user enrolled; reset that user's MFA (POST /auth/users/{id}/mfa/reset)")
}

// DisableMFA turns userID's MFA off, current being their password and code
// a code from their authenticator or a recovery code (so a stolen session
// and password alone do not remove the second factor): the factor and the
// recovery codes are deleted. It returns ErrMFANotEnabled,
// ErrWrongCurrentPassword (audited) and ErrInvalidMFACode (throttled).
func (s *Service) DisableMFA(ctx context.Context, userID uint, current, code string) error {
	if err := s.withSecondFactor(ctx, userID, current, code, func(tx *gorm.DB) error {
		return deleteMFATx(tx, userID)
	}); err != nil {
		return err
	}
	s.Audit(ctx, AuditEvent{Action: AuditMFADisabled, TargetUserID: uintPtr(userID)})
	return nil
}

// ResetMFA removes userID's MFA (the factor, pending or enabled, and the
// recovery codes), for a user who lost their authenticator and codes, and
// revokes every session of the user (the lost device may hold one). It
// checks no permission, like DisableUser: the HTTP route checks
// PermissionManageUsers. It returns ErrUserNotFound for an unknown userID;
// resetting an account without MFA changes nothing.
func (s *Service) ResetMFA(ctx context.Context, userID uint) error {
	return s.resetMFA(ctx, userID, nil)
}

func (s *Service) resetMFA(ctx context.Context, userID uint, check func(locked User) error) error {
	changed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := lockUserTx(tx, userID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errUserNotFound
		}
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(locked); err != nil {
				return err
			}
		}
		var n int64
		if err := tx.Model(&TOTPFactor{}).Where("user_id = ?", userID).Count(&n).Error; err != nil {
			return err
		}
		changed = n > 0
		if !changed {
			return nil
		}
		// A reset is asked for a lost authenticator, often lost with a
		// device signed in: every session of the user ends with it.
		if err := revokeAllTx(tx, userID, s.now()); err != nil {
			return err
		}
		return deleteMFATx(tx, userID)
	})
	if err != nil {
		return err
	}
	if changed {
		s.Audit(ctx, AuditEvent{Action: AuditMFAReset, TargetUserID: uintPtr(userID)})
	}
	return nil
}

func deleteMFATx(tx *gorm.DB, userID uint) error {
	if err := tx.Where("user_id = ?", userID).Delete(&TOTPFactor{}).Error; err != nil {
		return err
	}
	return tx.Where("user_id = ?", userID).Delete(&RecoveryCode{}).Error
}

func (s *Service) mfaRule(userID uint) throttleRule {
	return throttleRule{name: "MFAPerUser", limit: s.throttle.policy.MFAPerUser, subject: userSubject(userID)}
}
