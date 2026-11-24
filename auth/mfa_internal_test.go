package auth

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
)

// TestTOTPCodeRFC6238: the SHA-1 test vectors of RFC 6238 appendix B, to
// six digits.
func TestTOTPCodeRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := totpCode(key, unix/totpPeriod); got != want {
			t.Errorf("TOTP at %d = %s, want %s", unix, got, want)
		}
	}
}

// TestTOTPSecretSealing: a sealed secret opens for its user with the same
// key, and for no other user nor key.
func TestTOTPSecretSealing(t *testing.T) {
	cfg := config.AuthConfig{JWTSecret: strings.Repeat("j", 32)}
	s := &Service{cfg: cfg}
	sealed, err := s.sealTOTPSecret(7, []byte("secret-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "secret-bytes") {
		t.Fatal("the sealed secret shows the secret")
	}
	if got, err := s.openTOTPSecret(7, sealed); err != nil || string(got) != "secret-bytes" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := s.openTOTPSecret(8, sealed); !errors.Is(err, errMFASecretUnreadable) {
		t.Fatalf("open for another user = %v, want errMFASecretUnreadable", err)
	}
	other := &Service{cfg: config.AuthConfig{JWTSecret: cfg.JWTSecret, MFAKey: strings.Repeat("m", 32)}}
	if _, err := other.openTOTPSecret(7, sealed); !errors.Is(err, errMFASecretUnreadable) {
		t.Fatalf("open with another key = %v, want errMFASecretUnreadable", err)
	}
}

// TestUseMFACodeTxIsConditional: two requests that loaded the factor before
// either used the code (a stale LastStep each) cannot both pass: the step is
// recorded only where it is newer, so the second finds nothing to update.
func TestUseMFACodeTxIsConditional(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:" + filepath.Join(t.TempDir(), "mfa.db") + "?_fk=1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: db.DB, cfg: config.AuthConfig{JWTSecret: strings.Repeat("j", 32)}}
	secret := []byte("12345678901234567890")
	sealed, err := s.sealTOTPSecret(1, secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1234567890, 0)
	confirmed := now.Add(-time.Hour)
	f := TOTPFactor{UserID: 1, Secret: sealed, ConfirmedAt: &confirmed}
	if err := db.Create(&f).Error; err != nil {
		t.Fatal(err)
	}
	code := totpCode(secret, totpStep(now))
	stale := f // as both requests loaded it
	if _, err := s.useMFACodeTx(db.DB, 1, stale, code, now); err != nil {
		t.Fatalf("first use = %v", err)
	}
	if _, err := s.useMFACodeTx(db.DB, 1, stale, code, now); !errors.Is(err, errInvalidMFACode) {
		t.Fatalf("second use with the stale factor = %v, want errInvalidMFACode", err)
	}
}
