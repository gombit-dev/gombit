package auth

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
)

var (
	errPasswordRequired   = errors.New("password is required")
	errPasswordTooLong    = errors.New("password must be at most 72 bytes")
	errPasswordTooShort   = errors.New("password must be at least 8 characters")
	errInvalidCredentials = errors.New("invalid email or password")
	errEmailTaken         = errors.New("email already registered")
	errUserNotFound       = errors.New("user not found")
	errRefreshReuse       = errors.New("refresh token reuse detected")
	// errRotationRaced is a rotation that lost to another rotation of its
	// token, which gave the token a successor. Either it lost the
	// compare-and-swap to a rotation of the same token that another
	// instance ran (one instance joins its concurrent callers of a token to
	// one rotation), or it found the token expired and already given a
	// successor. It is an invalid refresh token, a 401; refreshCookie leaves
	// the cookies alone for it, because the winning rotation's response sets
	// the session's new ones in the same browser. A token revoked with no
	// successor (logout, a session revocation, the reuse cascade) is a plain
	// errInvalidRefreshToken, even when the revocation won the
	// compare-and-swap.
	errRotationRaced = fmt.Errorf("%w: rotated concurrently", errInvalidRefreshToken)
)

// ErrEmailTaken is the exported form of errEmailTaken. Register and
// CreateSuperuser return it (wrapped) so callers such as gombit
// createsuperuser can detect duplicates with errors.Is without a second
// uniqueness check.
var ErrEmailTaken = errEmailTaken

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// findUserByEmail loads the user whose email is email (normalized), exactly:
// the indexed lookup may match more on a database whose collation folds
// (MySQL's default, utf8mb4_0900_ai_ci, ignores zero-width characters and
// accents, and folds fullwidth letters), and a spelling the database folds
// is another email to everything else (the throttles count it as one). The
// unique index holds one row per folded email, so the check decides. It
// compares the stored email normalized, as the Service writes it: a row
// written another way ("Ada@Example.com", by an import or an app's own
// code) is still its account. A user of ID 0 is no account.
func findUserByEmail(db *gorm.DB, email string) (User, error) {
	var user User
	if err := db.Where("email = ?", email).Limit(1).Find(&user).Error; err != nil {
		return User{}, err
	}
	if normalizeEmail(user.Email) != email {
		return User{}, nil
	}
	return user, nil
}

// maxEmailLength is the users.email column's size, in characters.
const maxEmailLength = 255

// validEmail reports whether email is a bare RFC 5322 address (no display
// name, nothing around it) that fits the email column: what the routes'
// format:"email" and maxLength check, so a Go caller of the service gets
// the same answer as the API, on every database.
func validEmail(email string) bool {
	if email == "" || utf8.RuneCountInString(email) > maxEmailLength {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Name == "" && addr.Address == email
}
