package auth

import (
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const maxPasswordBytes = 72

// Hasher hashes and compares passwords.
type Hasher interface {
	Hash(password string) (string, error)
	Compare(hash, password string) error
}

type bcryptHasher struct {
	cost int
}

func newBcryptHasher(cost int) Hasher {
	if cost <= 0 {
		cost = bcrypt.DefaultCost
	}
	return bcryptHasher{cost: cost}
}

func (h bcryptHasher) Hash(password string) (string, error) {
	if err := checkPasswordLength(password); err != nil {
		return "", err
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), h.cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(hashed), nil
}

func (h bcryptHasher) Compare(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// minPasswordLength is the fewest characters a password a user sets
// themselves may have (MinLength in the routes' schemas is the same, for
// clients; this is the rule).
const minPasswordLength = 8

// passwordPolicyError is a new password the policy refuses, for the request
// field it came in (new_password for a change, password for a reset), so the
// error is reported where the form put it.
type passwordPolicyError struct {
	field string
	err   error
}

func (e passwordPolicyError) Error() string { return e.err.Error() }
func (e passwordPolicyError) Unwrap() error { return e.err }

// checkNewPassword is the policy for a password a user sets themselves (a
// change, a reset): at least minPasswordLength characters, and what the
// hasher can take (at most 72 bytes; a multibyte password reaches that in
// fewer characters). It is enforced here, at the service, whatever the
// transport validated; field names the request field for errors.
func checkNewPassword(password, field string) error {
	err := checkPasswordLength(password)
	if err == nil && utf8.RuneCountInString(password) < minPasswordLength {
		err = errPasswordTooShort
	}
	if err != nil {
		return passwordPolicyError{field: field, err: err}
	}
	return nil
}

func checkPasswordLength(password string) error {
	if password == "" {
		return errPasswordRequired
	}
	if len(password) > maxPasswordBytes {
		return errPasswordTooLong
	}
	return nil
}
