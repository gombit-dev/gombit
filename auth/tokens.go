package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const refreshTokenBytes = 32

var (
	errInvalidAccessToken  = errors.New("auth: invalid access token")
	errInvalidRefreshToken = errors.New("auth: invalid refresh token")
)

type accessClaims struct {
	Email     string `json:"email"`
	RefreshID uint   `json:"rid"`
	jwt.RegisteredClaims
}

func hashRefreshToken(token string) string { return hashOpaqueToken(token) }

func newRefreshToken() (string, error) { return newOpaqueToken() }

// hashOpaqueToken is the SHA-256 a random token is stored as: refresh
// tokens, and the account lifecycle's one-time tokens.
func hashOpaqueToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newOpaqueToken is 32 random bytes, hex-encoded.
func newOpaqueToken() (string, error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func signAccessToken(secret []byte, user User, refreshID uint, ttl time.Duration, now time.Time) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("auth: empty JWT secret")
	}
	claims := accessClaims{
		Email:     user.Email,
		RefreshID: refreshID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatUint(uint64(user.ID), 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			NotBefore: jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, nil
}

func parseAccessToken(secret []byte, token string, now time.Time) (accessClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithLeeway(0),
	)
	parsed, err := parser.ParseWithClaims(token, &accessClaims{}, func(t *jwt.Token) (any, error) {
		return secret, nil
	})
	if err != nil || parsed == nil || !parsed.Valid {
		return accessClaims{}, errInvalidAccessToken
	}
	claims, ok := parsed.Claims.(*accessClaims)
	if !ok || claims.Subject == "" || claims.RefreshID == 0 {
		return accessClaims{}, errInvalidAccessToken
	}
	return *claims, nil
}

func userIDFromSubject(subject string) (uint, error) {
	id, err := strconv.ParseUint(strings.TrimSpace(subject), 10, 64)
	if err != nil || id == 0 {
		return 0, errInvalidAccessToken
	}
	return uint(id), nil
}
