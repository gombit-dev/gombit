package auth

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
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
