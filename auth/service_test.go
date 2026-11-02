package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"golang.org/x/crypto/bcrypt"
)

func TestRotateRefreshKeepsNewAccessValid(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "auth.db") + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db.DB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.AccessTokenTTL = time.Minute
	cfg.Auth.RefreshTokenTTL = time.Hour
	svc, err := NewService(db.DB, cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	user, err := svc.Register(ctx, "ada@example.com", "correct-horse")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	first, err := svc.IssueTokens(ctx, user)
	if err != nil {
		t.Fatalf("IssueTokens: %v", err)
	}
	if _, err := svc.ParseAccess(ctx, first.AccessToken); err != nil {
		t.Fatalf("ParseAccess(first): %v", err)
	}

	second, err := svc.RotateRefresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("RotateRefresh: %v", err)
	}

	var rows []RefreshToken
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	for _, row := range rows {
		t.Logf("token id=%d user=%d revoked=%v replaced=%v", row.ID, row.UserID, row.RevokedAt, row.ReplacedBy)
	}

	if _, err := svc.ParseAccess(ctx, second.AccessToken); err != nil {
		t.Fatalf("ParseAccess(second): %v", err)
	}
	if _, err := svc.RotateRefresh(ctx, first.RefreshToken); err == nil {
		t.Fatal("old refresh should be invalid")
	}
}

func TestConcurrentRotateRefreshDoesNotRevokeWinner(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	user, err := svc.Register(ctx, "ada@example.com", "correct-horse")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	first, err := svc.IssueTokens(ctx, user)
	if err != nil {
		t.Fatalf("IssueTokens: %v", err)
	}

	type result struct {
		pair TokenPair
		err  error
	}
	const n = 8
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			pair, err := svc.RotateRefresh(ctx, first.RefreshToken)
			results[i] = result{pair: pair, err: err}
		}(i)
	}
	wg.Wait()

	var winner TokenPair
	wins := 0
	for i, r := range results {
		if r.err == nil {
			wins++
			winner = r.pair
			continue
		}
		if !errors.Is(r.err, errInvalidRefreshToken) && !errors.Is(r.err, errRefreshReuse) {
			t.Fatalf("RotateRefresh()[%d] error = %v, want success or invalid/reuse", i, r.err)
		}
	}
	if wins == 0 {
		t.Fatal("all concurrent RotateRefresh calls failed")
	}
	if _, err := svc.ParseAccess(ctx, winner.AccessToken); err != nil {
		t.Fatalf("ParseAccess(winner) error = %v; concurrent loser family-revoked the new session", err)
	}
}

// TestNewServiceRequiresRefreshTTLAtLeastAccessTTL: NewService refuses what
// config validation refuses, an access TTL above the refresh TTL, so a
// config.Config built without Validate cannot issue an access token that
// outlives its session either. Equal TTLs are fine.
func TestNewServiceRequiresRefreshTTLAtLeastAccessTTL(t *testing.T) {
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "auth.db") + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.AccessTokenTTL = time.Hour
	cfg.Auth.RefreshTokenTTL = time.Minute
	if _, err := NewService(db.DB, cfg); err == nil {
		t.Fatal("NewService with an access TTL above the refresh TTL succeeded, want an error")
	}
	cfg.Auth.RefreshTokenTTL = time.Hour
	if _, err := NewService(db.DB, cfg); err != nil {
		t.Fatalf("NewService with equal TTLs: %v", err)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "auth.db") + "?cache=shared&_fk=1",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db.DB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = "test-jwt-secret-32-bytes-minimum!"
	cfg.Auth.BcryptCost = bcrypt.MinCost
	svc, err := NewService(db.DB, cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestCreateSuperuserOnFreshDB is the M4-6 acceptance criterion: a
// createsuperuser call on a fresh database creates an admin (IsSuperuser)
// account whose password is bcrypt-hashed, not stored in plaintext.
func TestCreateSuperuserOnFreshDB(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateSuperuser(ctx, "Admin@Example.com", "correct-horse")
	if err != nil {
		t.Fatalf("CreateSuperuser: %v", err)
	}
	if !user.IsSuperuser {
		t.Fatal("CreateSuperuser: IsSuperuser = false, want true")
	}
	if user.Email != "admin@example.com" {
		t.Fatalf("CreateSuperuser: Email = %q, want normalized email", user.Email)
	}
	if user.PasswordHash == "correct-horse" {
		t.Fatal("CreateSuperuser: password stored in plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte("correct-horse")); err != nil {
		t.Fatalf("CreateSuperuser: password hash does not verify: %v", err)
	}

	var stored User
	if err := svc.db.First(&stored, user.ID).Error; err != nil {
		t.Fatalf("load stored user: %v", err)
	}
	if !stored.IsSuperuser {
		t.Fatal("stored user IsSuperuser = false, want true")
	}
}

// TestCreateUserAppliesTheAccountRules: Register and CreateSuperuser refuse
// an email the login body refuses (the account could never sign in) and a
// password outside the policy a user's own password meets; nothing is stored,
// and what the rules accept, at each boundary, is created (issue #461).
func TestCreateUserAppliesTheAccountRules(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	create := map[string]func(email, password string) (User, error){
		"Register":        func(e, p string) (User, error) { return svc.Register(ctx, e, p) },
		"CreateSuperuser": func(e, p string) (User, error) { return svc.CreateSuperuser(ctx, e, p) },
	}
	for name, fn := range create {
		for _, email := range []string{"admin", "Root Admin <root@example.com>", "a b c", "admin@", strings.Repeat("a", 250) + "@example.com"} {
			if _, err := fn(email, "correct-horse"); !errors.Is(err, ErrInvalidEmail) {
				t.Errorf("%s(%q) = %v, want ErrInvalidEmail", name, email, err)
			}
		}
		for password, reason := range map[string]error{"x": ErrPasswordTooShort, "seven77": ErrPasswordTooShort, strings.Repeat("p", 73): ErrPasswordTooLong} {
			var policy PasswordPolicyError
			if _, err := fn("rules-"+name+"@example.com", password); !errors.As(err, &policy) || !errors.Is(err, reason) || policy.Field != "password" {
				t.Errorf("%s(password of %d chars) = %v, want a PasswordPolicyError %v on password", name, len(password), err, reason)
			}
		}
	}
	var count int64
	if err := svc.db.Model(&User{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("users stored = %d (err %v), want 0", count, err)
	}
	// Seven runes is too short whatever the bytes.
	var policy PasswordPolicyError
	if _, err := svc.Register(ctx, "short@example.com", "ééééééé"); !errors.As(err, &policy) || !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("Register(7 two-byte runes) = %v, want ErrPasswordTooShort", err)
	}
	// Each limit's accepted side: 255 characters of address, 8 runes (16
	// bytes) and 72 bytes of password.
	for email, password := range map[string]string{
		strings.Repeat("a", 243) + "@example.com": "eight888",
		"runes@example.com":                      "éééééééé",
		"bytes@example.com":                      strings.Repeat("p", 72),
	} {
		if _, err := svc.Register(ctx, email, password); err != nil {
			t.Errorf("Register(%d-char email, %d-byte password) = %v, want it created", len(email), len(password), err)
		}
	}
	// What the rules accept signs in.
	if _, err := svc.CreateSuperuser(ctx, "admin@example.com", "eight888"); err != nil {
		t.Fatalf("CreateSuperuser(valid) = %v", err)
	}
	if _, err := svc.Authenticate(ctx, "admin@example.com", "eight888"); err != nil {
		t.Fatalf("Authenticate(the new superuser) = %v", err)
	}
}

// TestCreateSuperuserRefusesDuplicateEmail matches Register's uniqueness
// path (same errEmailTaken, exported as ErrEmailTaken).
func TestCreateSuperuserRefusesDuplicateEmail(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateSuperuser(ctx, "admin@example.com", "correct-horse"); err != nil {
		t.Fatalf("first CreateSuperuser: %v", err)
	}
	if _, err := svc.CreateSuperuser(ctx, "admin@example.com", "another-password"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("second CreateSuperuser error = %v, want ErrEmailTaken", err)
	}
	// Also refused against an existing non-superuser account for the same
	// email, and vice versa: the unique index does not distinguish roles.
	if _, err := svc.Register(ctx, "admin@example.com", "yet-another"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("Register against existing superuser email error = %v, want ErrEmailTaken", err)
	}
}

// TestRegisterDoesNotGrantSuperuser is the non-superuser register path
// acceptance criterion: /auth/register (Register) must not be a superuser
// escalation path.
func TestRegisterDoesNotGrantSuperuser(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	user, err := svc.Register(ctx, "user@example.com", "correct-horse")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.IsSuperuser {
		t.Fatal("Register: IsSuperuser = true, want false")
	}
	if _, err := svc.Register(ctx, "user@example.com", "correct-horse-2"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate Register error = %v, want ErrEmailTaken", err)
	}
}

// TestAuthenticateUnknownEmailParallel is the #113 regression: unknown-email
// logins lazily initialize Service.dummyHash. Concurrent Authenticate calls
// on one *Service must not race on that write (go test -race).
func TestAuthenticateUnknownEmailParallel(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Authenticate(ctx, "nobody@example.com", "x")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, errInvalidCredentials) {
			t.Fatalf("Authenticate()[%d] error = %v, want errInvalidCredentials", i, err)
		}
	}
}

func TestCreateSuperuserTable(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		password  string
		wantErr   error
		wantEmpty bool
	}{
		{name: "valid", email: "ada@example.com", password: "correct-horse"},
		{name: "empty email", email: "   ", password: "correct-horse", wantErr: errInvalidCredentials, wantEmpty: true},
		{name: "empty password", email: "grace@example.com", password: "", wantErr: errPasswordRequired, wantEmpty: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(t)
			user, err := svc.CreateSuperuser(context.Background(), tt.email, tt.password)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("CreateSuperuser() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateSuperuser() error = %v, want nil", err)
			}
			if !user.IsSuperuser {
				t.Fatal("CreateSuperuser() IsSuperuser = false, want true")
			}
		})
	}
}
