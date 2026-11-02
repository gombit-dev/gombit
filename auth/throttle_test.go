package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/auth/authtest"
	"github.com/gombit-dev/gombit/cache"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
)

// newThrottleEnv is newVerificationEnv with Service options (a policy, a
// store) and the auth config adjusted by tweak.
func newThrottleEnv(t *testing.T, db *database.DB, tweak func(*config.AuthConfig), opts ...auth.Option) verificationEnv {
	t.Helper()
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	cfg.Auth.EmailVerificationTTL = time.Hour
	if tweak != nil {
		tweak(&cfg.Auth)
	}
	mail := &authtest.Mailer{}
	svc, err := auth.NewService(db.DB, cfg, append([]auth.Option{auth.WithMailer(mail)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close(context.Background()) })
	clock := &stepClock{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	auth.SetClock(svc, clock)
	return verificationEnv{svc: svc, mail: mail, clock: clock, db: db}
}

// throttled asserts err is a refusal of rule, and returns its wait.
func throttled(t *testing.T, err error, rule string) time.Duration {
	t.Helper()
	var refused *auth.ThrottleError
	if !errors.Is(err, auth.ErrTooManyAttempts) || !errors.As(err, &refused) || refused.Rule != rule {
		t.Fatalf("err = %v, want a %s refusal", err, rule)
	}
	if refused.RetryAfter < time.Second {
		t.Fatalf("RetryAfter = %s, want at least a second", refused.RetryAfter)
	}
	return refused.RetryAfter
}

func TestThrottleService(t *testing.T) {
	runThrottleService(t, openSQLite(t))
}

func runThrottleService(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()
	from := func(ip string) context.Context { return auth.ContextWithAddress(ctx, ip) }

	t.Run("failed logins for one email from one address", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		user, _ := e.register(t, "throttle-login")
		attacker := from("198.51.100.7")
		for i := range 5 {
			if _, err := e.svc.Authenticate(attacker, user.Email, "wrong-horse"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("attempt %d = %v, want invalid credentials", i+1, err)
			}
		}
		// The sixth is refused before the password is checked: the right
		// one too, and an email typed differently is the same email.
		_, err := e.svc.Authenticate(attacker, strings.ToUpper(user.Email), testPassword)
		// The sliding window's estimate spreads a bucket's attempts over it,
		// so the wait is at most two windows.
		wait := throttled(t, err, "LoginPerAccountAddress")
		if wait > 30*time.Minute {
			t.Fatalf("RetryAfter = %s, want at most two windows", wait)
		}
		// Not a ban: the owner signs in from their own address, and the
		// attacker's address may still try other accounts.
		if _, err := e.svc.Authenticate(from("203.0.113.9"), user.Email, testPassword); err != nil {
			t.Fatalf("the owner from another address = %v", err)
		}
		if _, err := e.svc.Authenticate(attacker, "someone-else@example.com", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("another email from the attacker's address = %v, want invalid credentials", err)
		}
		// The window slides: once it has passed, the address may try again.
		e.clock.now = e.clock.now.Add(wait)
		if _, err := e.svc.Authenticate(attacker, user.Email, testPassword); err != nil {
			t.Fatalf("after RetryAfter = %v", err)
		}
	})

	t.Run("a spelling the database folds is not the account", func(t *testing.T) {
		// MySQL's default collation (utf8mb4_0900_ai_ci) finds the account
		// for these; the throttle counts each as another email. Only the
		// account's own email may authenticate, or every variant would be
		// a fresh allowance of guesses against it.
		e := newThrottleEnv(t, db, nil)
		user, _ := e.register(t, "throttle-folded")
		local, domain, _ := strings.Cut(user.Email, "@")
		variants := []string{
			local[:1] + "\u200b" + local[1:] + "@" + domain, // zero-width space
			local[:1] + "\u00ad" + local[1:] + "@" + domain, // soft hyphen
			local + "@" + strings.Replace(domain, "e", "\u00e9", 1),
			"\uff54" + local[1:] + "@" + domain, // fullwidth t
		}
		for _, variant := range variants {
			if _, err := e.svc.Authenticate(from("198.51.100.50"), variant, testPassword); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("Authenticate(%q) with the right password = %v, want invalid credentials", variant, err)
			}
			e.mail.Reset()
			if err := e.svc.RequestPasswordReset(ctx, variant); err != nil {
				t.Fatal(err)
			}
			if msgs := e.mail.Messages(); len(msgs) != 0 {
				t.Fatalf("a reset for %q mailed %+v", variant, msgs)
			}
		}
		if _, err := e.svc.Authenticate(from("198.51.100.50"), user.Email, testPassword); err != nil {
			t.Fatalf("the account's own email = %v", err)
		}
	})

	t.Run("a row written with another case is still its account", func(t *testing.T) {
		// Only a database whose collation folds case finds it by the
		// normalized email (MySQL's default); the others never did.
		if db.Name() != "mysql" {
			t.Skip("the lookup is case-sensitive on " + db.Name())
		}
		e := newThrottleEnv(t, db, nil)
		hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		email := fmt.Sprintf("Mixed.Case-%d@Example.com", time.Now().UnixNano())
		if err := e.db.Create(&auth.User{Email: email, PasswordHash: string(hash)}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Authenticate(from("203.0.113.60"), strings.ToLower(email), testPassword); err != nil {
			t.Fatalf("a mixed-case row, signing in with the normalized email = %v", err)
		}
	})

	t.Run("an account and an address that is none are throttled alike", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		attacker := from("198.51.100.8")
		for range 5 {
			if _, err := e.svc.Authenticate(attacker, "nobody@example.com", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Authenticate(attacker, "nobody@example.com", "x")
		throttled(t, err, "LoginPerAccountAddress")
	})

	t.Run("a successful login forgets the address's failures", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		user, _ := e.register(t, "throttle-forget")
		owner := from("203.0.113.10")
		for round := range 3 {
			for range 4 {
				if _, err := e.svc.Authenticate(owner, user.Email, "typo-horse"); !errors.Is(err, auth.ErrInvalidCredentials) {
					t.Fatalf("round %d: %v", round, err)
				}
			}
			if _, err := e.svc.Authenticate(owner, user.Email, testPassword); err != nil {
				t.Fatalf("round %d: the right password after 4 typos = %v", round, err)
			}
		}
	})

	t.Run("credential stuffing from one address", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.LoginPerAddress = auth.ThrottleLimit{Limit: 3, Window: time.Minute}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		stuffer := from("198.51.100.9")
		for i := range 3 {
			if _, err := e.svc.Authenticate(stuffer, fmt.Sprintf("victim-%d@example.com", i), "leaked"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Authenticate(stuffer, "victim-3@example.com", "leaked")
		throttled(t, err, "LoginPerAddress")
		if _, err := e.svc.Authenticate(from("198.51.100.10"), "victim-3@example.com", "leaked"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("another address = %v, want invalid credentials", err)
		}
	})

	t.Run("guessing spread over addresses", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.LoginPerAccount = auth.ThrottleLimit{Limit: 3, Window: time.Hour}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		user, _ := e.register(t, "throttle-spread")
		for i := range 3 {
			if _, err := e.svc.Authenticate(from(fmt.Sprintf("198.51.100.%d", 20+i)), user.Email, "guess"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Authenticate(from("198.51.100.99"), user.Email, "guess")
		throttled(t, err, "LoginPerAccount")
		// A Go caller (no request, no address) is held by the per-email rule.
		_, err = e.svc.Authenticate(ctx, user.Email, "guess")
		throttled(t, err, "LoginPerAccount")
	})

	t.Run("concurrent attempts cannot get past the limit together", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		user, _ := e.register(t, "throttle-burst")
		var compared atomic.Int64
		auth.SetHasher(e.svc, hookHasher{Hasher: auth.HasherOf(e.svc), before: func() { compared.Add(1) }})
		attacker := from("198.51.100.11")
		var wg sync.WaitGroup
		var refusedCount atomic.Int64
		for range 40 {
			wg.Go(func() {
				_, err := e.svc.Authenticate(attacker, user.Email, "burst")
				if errors.Is(err, auth.ErrTooManyAttempts) {
					refusedCount.Add(1)
				}
			})
		}
		wg.Wait()
		if compared.Load() > 5 || refusedCount.Load() < 35 {
			t.Fatalf("passwords compared = %d, refused = %d; want at most 5 compared", compared.Load(), refusedCount.Load())
		}
	})

	t.Run("invalid one-time tokens from one address", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.TokenPerAddress = auth.ThrottleLimit{Limit: 3, Window: time.Minute}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		user, verify := e.register(t, "throttle-token")
		prober := from("198.51.100.12")
		// A valid token does not count.
		if _, err := e.svc.VerifyEmail(prober, verify); err != nil {
			t.Fatal(err)
		}
		probes := []func() error{
			func() error { _, err := e.svc.VerifyEmail(prober, "made-up"); return err },
			func() error { _, err := e.svc.ResetPassword(prober, "made-up", "new-horse-1"); return err },
			func() error { _, err := e.svc.ConfirmEmailChange(prober, "made-up"); return err },
		}
		for i, probe := range probes {
			if err := probe(); !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("probe %d = %v, want ErrInvalidToken", i, err)
			}
		}
		for i, probe := range probes {
			throttled(t, probe(), "TokenPerAddress")
			_ = i
		}
		// Another address is not held.
		if _, err := e.svc.VerifyEmail(from("198.51.100.13"), "made-up"); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("another address = %v", err)
		}
		_ = user
	})

	t.Run("wrong current passwords of one user", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		user, _ := e.register(t, "throttle-current")
		for i := range 5 {
			var err error
			if i%2 == 0 {
				_, err = e.svc.ChangePassword(ctx, user.ID, "not-it", "brand-new-horse", auth.PasswordChange{})
			} else {
				err = e.svc.RequestEmailChange(ctx, user.ID, "not-it", fmt.Sprintf("new-%d@example.com", i))
			}
			if !errors.Is(err, auth.ErrWrongCurrentPassword) {
				t.Fatalf("attempt %d = %v", i, err)
			}
		}
		_, err := e.svc.ChangePassword(ctx, user.ID, testPassword, "brand-new-horse", auth.PasswordChange{})
		throttled(t, err, "CurrentPasswordPerUser")
		throttled(t, e.svc.RequestEmailChange(ctx, user.ID, testPassword, "new@example.com"), "CurrentPasswordPerUser")
		// Another user is not held, and a right password does not count.
		other, _ := e.register(t, "throttle-current-other")
		for range 6 {
			if _, err := e.svc.ChangePassword(ctx, other.ID, testPassword, testPassword+"!", auth.PasswordChange{}); err != nil {
				t.Fatalf("a right current password = %v", err)
			}
			if _, err := e.svc.ChangePassword(ctx, other.ID, testPassword+"!", testPassword, auth.PasswordChange{}); err != nil {
				t.Fatalf("a right current password = %v", err)
			}
		}
	})

	t.Run("password reset requests from one address", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.PasswordResetPerAddress = auth.ThrottleLimit{Limit: 2, Window: time.Minute}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		requester := from("198.51.100.14")
		for i := range 2 {
			if err := auth.RequestPasswordResetInBackground(requester, e.svc, fmt.Sprintf("any-%d@example.com", i)); err != nil {
				t.Fatal(err)
			}
		}
		throttled(t, auth.RequestPasswordResetInBackground(requester, e.svc, "any-2@example.com"), "PasswordResetPerAddress")
		if err := auth.RequestPasswordResetInBackground(from("198.51.100.15"), e.svc, "any-2@example.com"); err != nil {
			t.Fatalf("another address = %v", err)
		}
	})

	t.Run("verification messages of one user", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.VerificationPerUser = auth.ThrottleLimit{Limit: 3, Window: time.Hour}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		user, _ := e.register(t, "throttle-verify") // the first message
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		if err := e.svc.SendEmailVerification(ctx, user); err != nil {
			t.Fatal(err)
		}
		// The resend wait refuses this one, and it does not count.
		if err := e.svc.SendEmailVerification(ctx, user); err == nil || errors.Is(err, auth.ErrTooManyAttempts) {
			t.Fatalf("within a minute = %v, want the resend wait", err)
		}
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		if err := e.svc.SendEmailVerification(ctx, user); err != nil {
			t.Fatalf("the third message = %v", err)
		}
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		throttled(t, e.svc.SendEmailVerification(ctx, user), "VerificationPerUser")
	})

	t.Run("off when the config says so", func(t *testing.T) {
		e := newThrottleEnv(t, db, func(c *config.AuthConfig) { c.ThrottleDisabled = true })
		attacker := from("198.51.100.16")
		for range 10 {
			if _, err := e.svc.Authenticate(attacker, "nobody@example.com", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("disabled throttle = %v", err)
			}
		}
	})

	t.Run("a failing cache does not lock users out", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil, auth.WithThrottleStore(failingCache{}))
		user, _ := e.register(t, "throttle-fail-open")
		attacker := from("198.51.100.17")
		for range 8 {
			_, _ = e.svc.Authenticate(attacker, user.Email, "x")
		}
		if _, err := e.svc.Authenticate(attacker, user.Email, testPassword); err != nil {
			t.Fatalf("login with a failing throttle cache = %v", err)
		}
	})

	t.Run("an IPv6 client is counted by its /64", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		user, _ := e.register(t, "throttle-ipv6")
		// Five addresses of one /64 (one host's usual allocation).
		for i := range 5 {
			if _, err := e.svc.Authenticate(from(fmt.Sprintf("2001:db8:1:2::%x", i+1)), user.Email, "rotating"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Authenticate(from("2001:db8:1:2:ffff::1"), user.Email, "rotating")
		throttled(t, err, "LoginPerAccountAddress")
		// Another /64 is another client.
		if _, err := e.svc.Authenticate(from("2001:db8:1:3::1"), user.Email, testPassword); err != nil {
			t.Fatalf("another /64 = %v", err)
		}
	})

	t.Run("still throttling after Close", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil)
		if err := e.svc.Close(ctx); err != nil {
			t.Fatal(err)
		}
		attacker := from("198.51.100.19")
		for range 5 {
			if _, err := e.svc.Authenticate(attacker, "after-close@example.com", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Authenticate(attacker, "after-close@example.com", "x")
		throttled(t, err, "LoginPerAccountAddress")
	})

	t.Run("a refused request writes no counter", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.LoginPerAddress = auth.ThrottleLimit{Limit: 10, Window: 15 * time.Minute}
		store := &keyCounter{Cache: cache.NewMemory()}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p), auth.WithThrottleStore(store))
		attacker := from("198.51.100.60")
		refusals := 0
		for i := range 500 {
			_, err := e.svc.Authenticate(attacker, fmt.Sprintf("spray-%d@example.com", i), "x")
			if errors.Is(err, auth.ErrTooManyAttempts) {
				refusals++
			}
		}
		// The ten accepted attempts wrote three counters each (the
		// address's one shared); the 490 refused ones, none.
		if refusals != 490 || store.keys() > 21 {
			t.Fatalf("refusals = %d, counter keys written = %d; want 490 and at most 21", refusals, store.keys())
		}
		// Refused by a later rule (LoginPerAccount, after the address and
		// pair rules): every request a fresh address, none of them writes
		// the address or pair counters it names.
		p.LoginPerAccount = auth.ThrottleLimit{Limit: 3, Window: time.Hour}
		spread := &keyCounter{Cache: cache.NewMemory()}
		e = newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p), auth.WithThrottleStore(spread))
		for i := range 300 {
			_, _ = e.svc.Authenticate(from(fmt.Sprintf("10.%d.%d.1", i/250, i%250)), "spread@example.com", "x")
		}
		if spread.keys() > 7 {
			t.Fatalf("counter keys written by a spread refused per account = %d, want at most 7 (the 3 accepted attempts')", spread.keys())
		}
	})

	t.Run("a known client is not held by LoginPerAccount", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.LoginPerAccount = auth.ThrottleLimit{Limit: 3, Window: time.Hour}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		user, _ := e.register(t, "throttle-known")
		home := from("203.0.113.70")
		if _, err := e.svc.Authenticate(home, user.Email, testPassword); err != nil {
			t.Fatal(err)
		}
		// Others fill the account's rule from addresses of their own.
		for i := range 3 {
			_, _ = e.svc.Authenticate(from(fmt.Sprintf("198.51.100.%d", 70+i)), user.Email, "guess")
		}
		_, err := e.svc.Authenticate(from("198.51.100.79"), user.Email, testPassword)
		throttled(t, err, "LoginPerAccount")
		// The owner's own address signed in before: it still may.
		if _, err := e.svc.Authenticate(home, user.Email, testPassword); err != nil {
			t.Fatalf("the known address = %v", err)
		}
		// A reset empties the rule: a new address may try again.
		e.clock.now = e.clock.now.Add(2 * time.Minute)
		if _, err := e.svc.ResetPassword(ctx, e.resetToken(t, user), "a-reset-horse"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Authenticate(from("198.51.100.80"), user.Email, "a-reset-horse"); err != nil {
			t.Fatalf("a new address after the reset = %v", err)
		}
	})

	t.Run("registrations from one address", func(t *testing.T) {
		p := auth.DefaultThrottlePolicy()
		p.RegisterPerAddress = auth.ThrottleLimit{Limit: 2, Window: time.Hour}
		e := newThrottleEnv(t, db, nil, auth.WithThrottlePolicy(p))
		hasher := &countingHasher{}
		auth.SetHasher(e.svc, hasher)
		signup := from("198.51.100.90")
		for i := range 2 {
			if _, err := e.svc.Register(signup, fmt.Sprintf("signup-%d-%d@example.com", i, time.Now().UnixNano()), testPassword); err != nil {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Register(signup, fmt.Sprintf("signup-x-%d@example.com", time.Now().UnixNano()), testPassword)
		throttled(t, err, "RegisterPerAddress")
		if hasher.hashes.Load() != 2 {
			t.Fatalf("passwords hashed = %d, want 2: a refused registration hashes nothing", hasher.hashes.Load())
		}
		// A Go caller (no address) and another address are not held.
		if _, err := e.svc.Register(ctx, fmt.Sprintf("signup-go-%d@example.com", time.Now().UnixNano()), testPassword); err != nil {
			t.Fatalf("a Go caller = %v", err)
		}
	})

	t.Run("a cache that keeps nothing still throttles", func(t *testing.T) {
		e := newThrottleEnv(t, db, nil, auth.WithThrottleStore(cache.Namespace("app", cache.Noop{})))
		attacker := from("198.51.100.95")
		for range 5 {
			if _, err := e.svc.Authenticate(attacker, "noop-store@example.com", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := e.svc.Authenticate(attacker, "noop-store@example.com", "x")
		throttled(t, err, "LoginPerAccountAddress")
	})

	t.Run("instances sharing a cache share the counts", func(t *testing.T) {
		shared := cache.NewMemory()
		a := newThrottleEnv(t, db, nil, auth.WithThrottleStore(shared))
		b := newThrottleEnv(t, db, nil, auth.WithThrottleStore(shared))
		b.clock.now = a.clock.now
		attacker := from("198.51.100.18")
		for i := range 5 {
			env := a
			if i%2 == 1 {
				env = b
			}
			if _, err := env.svc.Authenticate(attacker, "shared@example.com", "x"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		_, err := b.svc.Authenticate(attacker, "shared@example.com", "x")
		throttled(t, err, "LoginPerAccountAddress")
	})
}

// keyCounter records the distinct keys written to a cache.
type keyCounter struct {
	cache.Cache
	mu      sync.Mutex
	written map[string]bool
}

func (k *keyCounter) note(key string) {
	if strings.HasPrefix(key, "auth:throttle:probe:") {
		return // the Service's one-time check of the store
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.written == nil {
		k.written = map[string]bool{}
	}
	k.written[key] = true
}

func (k *keyCounter) keys() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.written)
}

func (k *keyCounter) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	k.note(key)
	return k.Cache.Set(ctx, key, value, ttl)
}

func (k *keyCounter) IncrementTTL(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	k.note(key)
	return cache.IncrementTTL(ctx, k.Cache, key, delta, ttl)
}

// failingCache fails every call: a cache that is down.
type failingCache struct{}

var errCacheDown = errors.New("cache down")

func (failingCache) Get(context.Context, string, any) (bool, error)          { return false, errCacheDown }
func (failingCache) Set(context.Context, string, any, time.Duration) error   { return errCacheDown }
func (failingCache) Delete(context.Context, ...string) error                 { return errCacheDown }
func (failingCache) Increment(context.Context, string, int64) (int64, error) { return 0, errCacheDown }

// TestThrottleHTTP: a throttled login is a 429 with Retry-After, and the
// client address is the one GOMBIT_HTTP_TRUSTED_PROXIES says: a trusted
// proxy's X-Forwarded-For names the client; an untrusted one's is ignored.
func TestThrottleHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultFor(config.EnvironmentTest)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.TrustedProxies = []string{"192.0.2.1"} // httptest's RemoteAddr
	cfg.Auth.JWTSecret = testJWTSecret
	cfg.Auth.BcryptCost = bcrypt.MinCost
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()))
	if err != nil {
		t.Fatal(err)
	}
	login := func(forwardedFor string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"http-throttle@example.com","password":"wrong-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		if forwardedFor != "" {
			req.Header.Set("X-Forwarded-For", forwardedFor)
		}
		app.Router().ServeHTTP(rec, req)
		return rec
	}
	for range 5 {
		if rec := login("198.51.100.30"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a failed login = %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := login("198.51.100.30")
	assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 1800 {
		t.Fatalf("Retry-After = %q, want 1..1800 seconds (at most two windows)", rec.Header().Get("Retry-After"))
	}
	// The forwarded client is the subject, not the proxy: another client
	// behind the same proxy is not held.
	if rec := login("198.51.100.31"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another client behind the trusted proxy = %d, want 401", rec.Code)
	}

	// The forgot route: 202 until the address's limit, then 429 (the 503
	// stays the background runner's).
	forgot := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/forgot", strings.NewReader(`{"email":"anyone@example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100.32")
		app.Router().ServeHTTP(rec, req)
		return rec
	}
	limit := auth.DefaultThrottlePolicy().PasswordResetPerAddress.Limit
	for i := range limit {
		if rec := forgot(); rec.Code != http.StatusAccepted {
			t.Fatalf("forgot %d = %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec = forgot()
	assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a throttled forgot request has no Retry-After")
	}
}

// TestThrottleIgnoresUntrustedForwardedFor: with no trusted proxy, a client
// cannot pick a fresh address per attempt with X-Forwarded-For.
func TestThrottleIgnoresUntrustedForwardedFor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	app := newVerificationApp(t, db, config.AuthModeJWT, nil, config.EnvironmentTest)
	for i := range 6 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"spoof@example.com","password":"wrong-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", 40+i))
		app.Router().ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("attempt %d with a spoofed X-Forwarded-For = %d, want %d", i+1, rec.Code, want)
		}
	}
}

// TestThrottledRoutesOpenAPI: every throttled route declares its 429 with an
// integer Retry-After, and keeps the default error response, in both modes.
func TestThrottledRoutesOpenAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	paths := []string{
		"/api/v1/auth/register",
		"/api/v1/auth/login",
		"/api/v1/auth/password/forgot",
		"/api/v1/auth/password/reset",
		"/api/v1/auth/password/change",
		"/api/v1/auth/verify-email",
		"/api/v1/auth/verify-email/resend",
		"/api/v1/auth/email/change",
		"/api/v1/auth/email/change/confirm",
	}
	for name, newApp := range map[string]func(*testing.T) *framework.App{"bearer": newAuthApp, "cookie": newCookieAuthApp} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newApp(t).Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
			var spec struct {
				Paths map[string]map[string]struct {
					Responses map[string]struct {
						Headers map[string]struct {
							Schema struct {
								Type    string  `json:"type"`
								Minimum float64 `json:"minimum"`
							} `json:"schema"`
						} `json:"headers"`
					} `json:"responses"`
				} `json:"paths"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
				t.Fatalf("decode OpenAPI: %v", err)
			}
			for _, path := range paths {
				responses := spec.Paths[path]["post"].Responses
				if _, ok := responses["default"]; !ok {
					t.Errorf("%s: no default error response", path)
				}
				retryAfter, ok := responses["429"].Headers["Retry-After"]
				if !ok || retryAfter.Schema.Type != "integer" || retryAfter.Schema.Minimum != 1 {
					t.Errorf("%s: 429 Retry-After = %+v (declared %v), want an integer of at least 1", path, retryAfter.Schema, ok)
				}
			}
		})
	}
}

// TestThrottleUsesTheAppCache: under framework.App the counts live in the
// app's cache, so two instances on one cache (Redis in production) share
// them; a noop cache still throttles, in the Service's own memory.
func TestThrottleUsesTheAppCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := openSQLite(t)
	if err := auth.Migrate(db.DB); err != nil {
		t.Fatal(err)
	}
	newApp := func(c cache.Cache) *framework.App {
		cfg := config.DefaultFor(config.EnvironmentTest)
		cfg.HTTP.Addr = "127.0.0.1:0"
		cfg.Auth.JWTSecret = testJWTSecret
		cfg.Auth.BcryptCost = bcrypt.MinCost
		app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db), framework.WithLogger(zap.NewNop()), framework.WithCache(c))
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	login := func(app *framework.App, email string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"`+email+`","password":"wrong-horse"}`))
		req.Header.Set("Content-Type", "application/json")
		app.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	shared := cache.NewMemory()
	a, b := newApp(shared), newApp(shared)
	for i := range 5 {
		app := a
		if i%2 == 1 {
			app = b
		}
		if code := login(app, "fleet@example.com"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d", i+1, code)
		}
	}
	if code := login(b, "fleet@example.com"); code != http.StatusTooManyRequests {
		t.Fatalf("the sixth failure on the other instance = %d, want 429", code)
	}
	noop := newApp(cache.Noop{})
	for range 5 {
		login(noop, "noop@example.com")
	}
	if code := login(noop, "noop@example.com"); code != http.StatusTooManyRequests {
		t.Fatalf("with a noop app cache = %d, want 429 (the Service's own memory)", code)
	}
}
