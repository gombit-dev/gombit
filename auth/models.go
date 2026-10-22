package auth

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// User is the auth identity. IsSuperuser bypasses every permission check;
// regular users receive permissions directly or through groups.
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Email        string `gorm:"uniqueIndex;size:255;not null"`
	PasswordHash string `gorm:"not null"`
	IsSuperuser  bool   `gorm:"not null;default:false"`
	// EmailVerifiedAt is when the user proved they own VerifiedEmail (POST
	// /auth/verify-email); nil while no address is verified. A superuser
	// created by gombit createsuperuser starts verified.
	EmailVerifiedAt *time.Time
	// VerifiedEmail is the address that proof was for. The verification
	// holds only while it is Email: changing Email, by any path (an update
	// through GORM, the admin), makes the user unverified until the new
	// address is proven.
	VerifiedEmail string       `gorm:"size:255;not null;default:''"`
	Groups        []Group      `gorm:"many2many:auth_user_groups;"`
	Permissions   []Permission `gorm:"many2many:auth_user_permissions;"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// EmailVerified reports whether the user has verified their current email:
// a verification exists, and it is for the address the user has now.
func (u User) EmailVerified() bool {
	return u.EmailVerifiedAt != nil && u.VerifiedEmail != "" && normalizeEmail(u.VerifiedEmail) == normalizeEmail(u.Email)
}

// Group collects permissions that can be assigned to users.
type Group struct {
	ID          uint         `gorm:"primaryKey"`
	Name        string       `gorm:"uniqueIndex;not null;size:100"`
	Permissions []Permission `gorm:"many2many:auth_group_permissions;"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Permission is a stable authorization key, such as admin.widgets.view.
type Permission struct {
	ID          uint   `gorm:"primaryKey"`
	Key         string `gorm:"column:permission_key;uniqueIndex;not null;size:120"` // not "key": reserved in MySQL
	Description string `gorm:"size:255"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RefreshToken is a hashed, rotating refresh credential.
type RefreshToken struct {
	ID         uint       `gorm:"primaryKey"`
	UserID     uint       `gorm:"index;not null"`
	TokenHash  string     `gorm:"uniqueIndex;size:64;not null"`
	ExpiresAt  time.Time  `gorm:"index;not null"`
	RevokedAt  *time.Time `gorm:"index"`
	ReplacedBy *uint
	CreatedAt  time.Time
}

// TokenPurpose is what a OneTimeToken is for.
type TokenPurpose string

const (
	// PurposeVerifyEmail proves the user owns the token's email (AUTH-1).
	PurposeVerifyEmail TokenPurpose = "verify_email"
	// PurposeResetPassword lets the holder set a new password (AUTH-2).
	PurposeResetPassword TokenPurpose = "reset_password"
)

// OneTimeToken is a hashed, expiring, single-use token the account
// lifecycle mails to a user: email verification (AUTH-1), and the reset and
// change flows that build on it. The raw token exists only in the message;
// the row keeps its SHA-256. Email is the address it was issued for: a token
// proves ownership of that address only, so it stops working once the
// user's email is another one.
//
// A token is pending until the message carrying it was delivered to the
// Mailer (SentAt): a pending token does not work. Delivery activates it and
// supersedes the user's earlier tokens for the purpose in one transaction,
// so no failure leaves an undelivered token working, or two delivered ones.
type OneTimeToken struct {
	ID        uint         `gorm:"primaryKey"`
	UserID    uint         `gorm:"not null;index:idx_auth_one_time_tokens_user_purpose,priority:1"`
	Purpose   TokenPurpose `gorm:"size:32;not null;index:idx_auth_one_time_tokens_user_purpose,priority:2"`
	TokenHash string       `gorm:"uniqueIndex;size:64;not null"`
	Email     string       `gorm:"size:255;not null"`
	ExpiresAt time.Time    `gorm:"index;not null"`
	// SentAt is when the message carrying the token was delivered to the
	// Mailer; nil while it is pending.
	SentAt *time.Time
	// UsedAt is when the token was consumed, or superseded by a newer one
	// for the same purpose; nil while it can still be used.
	UsedAt    *time.Time
	CreatedAt time.Time
}

// TableName keeps the table in the auth_ namespace.
func (OneTimeToken) TableName() string { return "auth_one_time_tokens" }

// Models returns GORM models for Atlas / AutoMigrate.
func Models() []any {
	return []any{&User{}, &RefreshToken{}, &Group{}, &Permission{}, &OneTimeToken{}, &AuditEvent{}}
}

// Migrate creates the auth identity, token, group, permission, one-time
// token and audit tables.
func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("auth: nil database")
	}
	return db.AutoMigrate(Models()...)
}
