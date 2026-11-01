package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gombit-dev/gombit/cache"
	"go.uber.org/zap"
)

// Abuse protection for the credential endpoints (AUTH-8, #338). Each rule
// bounds the attempts of one subject (a client address, an email as typed,
// a user) in a sliding window, counted in the Service's cache (the app's
// cache under framework.App, Redis to share it between instances; a
// process-local memory cache otherwise).
//
// An attempt reserves its place before the costly work (the lookup, bcrypt,
// the token's transaction), atomically, so concurrent attempts cannot get
// past a limit together; the ones the rule does not count (a successful
// login, a valid token) are refunded after. A rule over its limit answers
// ErrTooManyAttempts with the wait, before any of that work, and the same
// for an email that has an account and one that has none: the subject is
// what was typed, never the account.
//
// The client address is the request's (gin's ClientIP, so
// GOMBIT_HTTP_TRUSTED_PROXIES decides which forwarding headers count). No
// rule bans an address outright: the per-address limits are wide, for the
// users behind one NAT or proxy, and count failures only; the tight limit
// is per address and email together. A call with no request (a Go caller)
// has no address, and only the other rules apply.
//
// A cache that fails does not lock users out: the attempt goes on, as if
// unthrottled, and a warning is logged (at most once a minute).

// ThrottleLimit allows Limit attempts in any Window. A zero Limit or Window
// turns the rule off.
type ThrottleLimit struct {
	Limit  int
	Window time.Duration
}

func (l ThrottleLimit) on() bool { return l.Limit > 0 && l.Window > 0 }

// ThrottlePolicy holds the credential endpoints' limits.
// DefaultThrottlePolicy is the framework's; WithThrottlePolicy replaces it.
type ThrottlePolicy struct {
	// LoginPerAccountAddress bounds failed logins for one email from one
	// client address: the guessing of one account's password.
	LoginPerAccountAddress ThrottleLimit
	// LoginPerAddress bounds failed logins from one client address, for any
	// email: credential stuffing. Wide, for many users behind one address.
	LoginPerAddress ThrottleLimit
	// LoginPerAccount bounds failed logins for one email from any address: a
	// guessing spread over many addresses. Wide, since anyone can spend it
	// (the account's owner then waits too, for at most its window).
	LoginPerAccount ThrottleLimit
	// PasswordResetPerAddress bounds password reset requests (POST
	// /auth/password/forgot) from one client address, whatever the email:
	// mail sent to many addresses. Each address gets one message a minute
	// anyway (the resend wait).
	PasswordResetPerAddress ThrottleLimit
	// TokenPerAddress bounds the invalid one-time tokens (email
	// verification, password reset, email change confirmation) posted from
	// one client address.
	TokenPerAddress ThrottleLimit
	// CurrentPasswordPerUser bounds the wrong current passwords one user
	// gives (password change, email change): the guessing of a password
	// from a stolen session.
	CurrentPasswordPerUser ThrottleLimit
	// VerificationPerUser bounds the verification messages one user is sent,
	// on top of the resend wait (one a minute).
	VerificationPerUser ThrottleLimit
}

// DefaultThrottlePolicy returns the framework's limits.
func DefaultThrottlePolicy() ThrottlePolicy {
	return ThrottlePolicy{
		LoginPerAccountAddress:  ThrottleLimit{Limit: 5, Window: 15 * time.Minute},
		LoginPerAddress:         ThrottleLimit{Limit: 100, Window: 15 * time.Minute},
		LoginPerAccount:         ThrottleLimit{Limit: 100, Window: time.Hour},
		PasswordResetPerAddress: ThrottleLimit{Limit: 20, Window: 15 * time.Minute},
		TokenPerAddress:         ThrottleLimit{Limit: 30, Window: 15 * time.Minute},
		CurrentPasswordPerUser:  ThrottleLimit{Limit: 5, Window: 15 * time.Minute},
		VerificationPerUser:     ThrottleLimit{Limit: 10, Window: time.Hour},
	}
}

// ErrTooManyAttempts is the error of an attempt a throttle refused; the
// error is a *ThrottleError, which says how long to wait.
var ErrTooManyAttempts = errors.New("auth: too many attempts; try again later")

// ThrottleError is an attempt refused by Rule, a ThrottlePolicy field's
// name, until RetryAfter has passed. errors.Is(err, ErrTooManyAttempts).
type ThrottleError struct {
	Rule       string
	RetryAfter time.Duration
}

func (e *ThrottleError) Error() string {
	return fmt.Sprintf("%v (%s; retry after %s)", ErrTooManyAttempts, e.Rule, e.RetryAfter)
}

// Is reports ErrTooManyAttempts.
func (e *ThrottleError) Is(target error) bool { return target == ErrTooManyAttempts }

// WithThrottlePolicy replaces the credential endpoints' limits
// (DefaultThrottlePolicy).
func WithThrottlePolicy(p ThrottlePolicy) Option {
	return func(s *Service) { s.throttle.policy = p }
}

// WithThrottleStore keeps the throttles' counters in c: an app's cache
// (Redis, to share them between instances). The Service does not close it.
// Without one, it keeps them in a process-local memory cache of its own,
// which Close stops.
func WithThrottleStore(c cache.Cache) Option {
	return func(s *Service) {
		if c != nil {
			s.throttle.store = c
		}
	}
}

// throttleRule is one rule applied to one subject.
type throttleRule struct {
	name    string
	limit   ThrottleLimit
	subject string
}

// throttler counts attempts. Its zero store means its own memory cache,
// made on first use.
type throttler struct {
	policy   ThrottlePolicy
	store    cache.Cache
	disabled bool
	now      func() time.Time
	logger   func() *zap.Logger

	ownMu     sync.Mutex
	own       *cache.Memory
	ownClosed bool
	// warned is when a store failure was last logged (unix seconds).
	warned atomic.Int64
	// refusing is, per rule, when a refusal was last logged.
	refusingMu sync.Mutex
	refusing   map[string]time.Time
}

func (t *throttler) cache() cache.Cache {
	if t.store != nil {
		return t.store
	}
	t.ownMu.Lock()
	defer t.ownMu.Unlock()
	if t.own == nil {
		if t.ownClosed {
			// After Close: still counting, but with no janitor to leak.
			t.own = cache.NewMemory()
		} else {
			t.own = cache.NewMemory(cache.WithJanitor(time.Minute))
		}
	}
	return t.own
}

// close stops the throttler's own memory cache, if it made one.
func (t *throttler) close() {
	t.ownMu.Lock()
	defer t.ownMu.Unlock()
	t.ownClosed = true
	if t.own != nil {
		_ = t.own.Close()
	}
}

// reservation is the places an attempt took, to refund.
type reservation struct {
	t    *throttler
	keys []reservedKey
}

type reservedKey struct {
	rule string
	key  string
	ttl  time.Duration
}

// reserve takes a place in every rule that is on and has a subject. When a
// rule is over its limit it refunds them all and returns a *ThrottleError
// with the longest wait.
func (t *throttler) reserve(ctx context.Context, rules ...throttleRule) (*reservation, error) {
	res := &reservation{t: t}
	if t.disabled {
		return res, nil
	}
	var refused *ThrottleError
	for _, rule := range rules {
		if !rule.limit.on() || rule.subject == "" {
			continue
		}
		key, wait, err := t.take(ctx, rule)
		if err != nil {
			t.warn(err)
			continue
		}
		if wait > 0 {
			if refused == nil || wait > refused.RetryAfter {
				refused = &ThrottleError{Rule: rule.name, RetryAfter: wait}
			}
			continue
		}
		res.keys = append(res.keys, key)
	}
	if refused != nil {
		res.refund()
		t.logRefusal(refused.Rule)
		return nil, refused
	}
	return res, nil
}

// logRefusal says, at most once a minute per rule, that a rule is refusing
// attempts: an operator sees an attack (or a limit too tight for the app's
// users) without a log line per request. No subject is logged.
func (t *throttler) logRefusal(rule string) {
	now := time.Now()
	t.refusingMu.Lock()
	if t.refusing == nil {
		t.refusing = map[string]time.Time{}
	}
	last, seen := t.refusing[rule]
	log := !seen || now.Sub(last) >= time.Minute
	if log {
		t.refusing[rule] = now
	}
	t.refusingMu.Unlock()
	if log {
		t.logger().Info("auth: a throttle is refusing attempts (logged at most once a minute)", zap.String("rule", rule))
	}
}

// take counts one attempt for rule in the current window's bucket and
// estimates the sliding window's count from it and the previous bucket,
// weighted by how much of that one the window still covers. Over the limit,
// it takes the attempt back and returns the wait.
func (t *throttler) take(ctx context.Context, rule throttleRule) (reservedKey, time.Duration, error) {
	window := rule.limit.Window
	now := t.now()
	index := now.UnixNano() / int64(window)
	base := throttleKeyBase(rule)
	current := reservedKey{rule: rule.name, key: base + strconv.FormatInt(index, 10), ttl: 2 * window}
	store := t.cache()
	count, err := cache.IncrementTTL(ctx, store, current.key, 1, current.ttl)
	if err != nil {
		return reservedKey{}, 0, err
	}
	var previous int64
	if _, err := store.Get(ctx, base+strconv.FormatInt(index-1, 10), &previous); err != nil {
		t.refundKey(current)
		return reservedKey{}, 0, err
	}
	elapsed := float64(now.UnixNano()-index*int64(window)) / float64(window)
	limit := float64(rule.limit.Limit)
	if float64(max(previous, 0))*(1-elapsed)+float64(count) <= limit {
		return current, 0, nil
	}
	t.refundKey(current)
	return reservedKey{}, throttleWait(float64(max(previous, 0)), float64(count-1), limit, elapsed, window), nil
}

// throttleWait is how long until one more attempt fits: previous decays as
// the window slides past its bucket; current becomes the previous bucket in
// the next window.
func throttleWait(previous, current, limit, elapsed float64, window time.Duration) time.Duration {
	var at float64 // the window fraction, from this bucket's start, it fits at
	if current+1 <= limit {
		at = 1 - (limit-current-1)/previous
	} else {
		at = 1 + 1 - (limit-1)/current
	}
	wait := time.Duration((at - elapsed) * float64(window))
	return max(wait.Round(time.Second), time.Second)
}

// clear forgets rule's attempts: its current and previous buckets.
func (t *throttler) clear(ctx context.Context, rule throttleRule) {
	if t.disabled || !rule.limit.on() || rule.subject == "" {
		return
	}
	index := t.now().UnixNano() / int64(rule.limit.Window)
	base := throttleKeyBase(rule)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), throttleRefundTimeout)
	defer cancel()
	if err := t.cache().Delete(ctx, base+strconv.FormatInt(index, 10), base+strconv.FormatInt(index-1, 10)); err != nil {
		t.warn(err)
	}
}

// throttleRefundTimeout bounds a refund or a clear, which run even when the
// request is gone.
const throttleRefundTimeout = 2 * time.Second

// refund gives the reservation's places back: the attempt did not count.
func (r *reservation) refund() { r.refundExcept("") }

// refundExcept refunds every place but the one of rule (a rule about to be
// cleared, whose refund would be a wasted write).
func (r *reservation) refundExcept(rule string) {
	if r == nil {
		return
	}
	for _, key := range r.keys {
		if key.rule != rule {
			r.t.refundKey(key)
		}
	}
	r.keys = nil
}

func (t *throttler) refundKey(key reservedKey) {
	ctx, cancel := context.WithTimeout(context.Background(), throttleRefundTimeout)
	defer cancel()
	// IncrementTTL, not Increment: should the bucket have expired meanwhile,
	// the decrement must not leave a counter that never does.
	if _, err := cache.IncrementTTL(ctx, t.cache(), key.key, -1, key.ttl); err != nil {
		t.warn(err)
	}
}

func (t *throttler) warn(err error) {
	now := time.Now().Unix()
	last := t.warned.Load()
	if now-last < 60 || !t.warned.CompareAndSwap(last, now) {
		return
	}
	t.logger().Warn("auth: the throttle's cache failed; attempts go on unthrottled", zap.Error(err))
}

// throttleKeyBase names a rule's buckets for a subject, hashed: no email or
// address is written to the cache.
func throttleKeyBase(rule throttleRule) string {
	sum := sha256.Sum256([]byte(rule.name + "\x00" + rule.subject))
	return "auth:throttle:" + rule.name + ":" + hex.EncodeToString(sum[:16]) + ":"
}

// requestAddress is the client address the request info middleware
// captured, empty outside a request.
func requestAddress(ctx context.Context) string {
	if info, ok := ctx.Value(requestInfoKey{}).(requestInfo); ok {
		return addressSubject(info.ip)
	}
	return ""
}

// addressSubject is the client a per-address rule counts: the IPv4
// address, or the IPv6 /64 the address is in (one host's usual
// allocation, so a client cannot take a fresh subject per attempt from its
// own prefix). An address that does not parse counts as written.
func addressSubject(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

func userSubject(id uint) string { return "user:" + strconv.FormatUint(uint64(id), 10) }

// loginRules are the rules a login attempt for email counts in.
func (s *Service) loginRules(ctx context.Context, email string) (pair throttleRule, all []throttleRule) {
	p, address := s.throttle.policy, requestAddress(ctx)
	if email == "" {
		email = "\x00" // still one subject: the empty email is typed too
	}
	pair = throttleRule{name: "LoginPerAccountAddress", limit: p.LoginPerAccountAddress}
	if address != "" {
		pair.subject = address + "\x00" + email
	}
	return pair, []throttleRule{
		pair,
		{name: "LoginPerAddress", limit: p.LoginPerAddress, subject: address},
		{name: "LoginPerAccount", limit: p.LoginPerAccount, subject: email},
	}
}

func (s *Service) tokenRule(ctx context.Context) throttleRule {
	return throttleRule{name: "TokenPerAddress", limit: s.throttle.policy.TokenPerAddress, subject: requestAddress(ctx)}
}

func (s *Service) currentPasswordRule(userID uint) throttleRule {
	return throttleRule{name: "CurrentPasswordPerUser", limit: s.throttle.policy.CurrentPasswordPerUser, subject: userSubject(userID)}
}

// throttleTokenUse runs use, a one-time token's consumption, as one
// attempt of the TokenPerAddress rule: only an invalid token counts.
func throttleTokenUse[T any](ctx context.Context, s *Service, use func() (T, error)) (T, error) {
	res, err := s.throttle.reserve(ctx, s.tokenRule(ctx))
	if err != nil {
		var zero T
		return zero, err
	}
	v, err := use()
	if !errors.Is(err, errInvalidOneTimeToken) {
		res.refund()
	}
	return v, err
}

// throttleRetryAfterDoc describes a throttled route's Retry-After header.
const throttleRetryAfterDoc = "Seconds until another attempt is accepted"

// throttleRetryAfterSeconds is a refusal's Retry-After value.
func throttleRetryAfterSeconds(e *ThrottleError) int {
	return max(int(math.Ceil(e.RetryAfter.Seconds())), 1)
}
