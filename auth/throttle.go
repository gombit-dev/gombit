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
// what was typed (normalized), and only the account's exact email is the
// account (findUserByEmail), so a spelling a database's collation folds is
// no fresh allowance. Every rule is read before any is counted, so a
// refused request writes nothing for the subjects it chose.
//
// The client address is the request's (gin's ClientIP, so
// GOMBIT_HTTP_TRUSTED_PROXIES decides which forwarding headers count). No
// rule bans an address outright: the per-address limits are wide, for the
// users behind one NAT or proxy, and count failures only; the tight limit
// is per address and email together. A call with no request (a Go caller)
// has no address, and only the other rules apply.
//
// A cache that fails does not lock users out: the attempt goes on, as if
// unthrottled, and a warning is logged (at most once a minute). A cache
// that keeps nothing (an increment answering 0) is replaced by the own
// memory cache, with a warning.

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
	// guessing spread over many addresses. Anyone can spend it: a handful of
	// addresses failing at their own limit keep it full for as long as they
	// go on, and then every client is refused for the account but the known
	// ones (an address that signed in to it before: KnownClientTTL). A
	// password reset empties it.
	LoginPerAccount ThrottleLimit
	// KnownClientTTL is how long a successful login exempts its client
	// address from LoginPerAccount for that email, renewed by each one.
	// Zero exempts no client.
	KnownClientTTL time.Duration
	// RegisterPerAddress bounds registrations (POST /auth/register) from one
	// client address, every request counted: each one hashes a password and
	// mails a verification message to the address it names.
	RegisterPerAddress ThrottleLimit
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
		KnownClientTTL:          30 * 24 * time.Hour,
		RegisterPerAddress:      ThrottleLimit{Limit: 20, Window: time.Hour},
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
	// probed is set once a probe of store decided (storeKeepsNothing);
	// keepsNothing is its answer: the counts go to the own memory
	// cache instead. A probe that cannot reach the store decides nothing,
	// and runs again on the next use.
	probeMu      sync.Mutex
	probed       atomic.Bool
	keepsNothing atomic.Bool
	// warned is when a store failure was last logged (unix seconds).
	warned atomic.Int64
	// refusing is, per rule, when a refusal was last logged.
	refusingMu sync.Mutex
	refusing   map[string]time.Time
}

func (t *throttler) cache() cache.Cache {
	if t.store != nil && !t.storeKeepsNothing() {
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

// storeKeepsNothing probes store once: a counter written to a key of its
// own, read back, then deleted. A store that answers but keeps nothing (a
// noop cache, however wrapped) would turn every rule off without a word.
func (t *throttler) storeKeepsNothing() bool {
	if t.probed.Load() {
		return t.keepsNothing.Load()
	}
	t.probeMu.Lock()
	defer t.probeMu.Unlock()
	if t.probed.Load() {
		return t.keepsNothing.Load()
	}
	ctx, cancel := context.WithTimeout(context.Background(), throttleRefundTimeout)
	defer cancel()
	key := "auth:throttle:probe:" + strconv.FormatInt(time.Now().UnixNano(), 36) + ":" + strconv.FormatUint(throttleProbeSeq.Add(1), 36)
	count, err := cache.IncrementTTL(ctx, t.store, key, 1, time.Minute)
	var stored int64
	var found bool
	if err == nil {
		found, err = t.store.Get(ctx, key, &stored)
	}
	if err != nil {
		t.warn(err) // undecided: probe again next time
		return false
	}
	_ = t.store.Delete(ctx, key)
	if count != 1 || !found || stored != 1 {
		t.keepsNothing.Store(true)
		t.logger().Warn("auth: the throttle's cache keeps no counts (a noop cache?); counting in process memory instead")
	}
	t.probed.Store(true)
	return t.keepsNothing.Load()
}

// throttleProbeSeq tells apart the probe keys of one process.
var throttleProbeSeq atomic.Uint64

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

// reserve takes a place in every rule that is on and has a subject, in two
// steps. First it reads them all: a rule already at its limit refuses the
// attempt with nothing written, so a refused request (the attacker's, at
// full rate) allocates no counter for the subjects it chose. Then it
// counts the attempt in each, atomically: one that went over meanwhile (a
// concurrent attempt took the last place) refunds them all and refuses.
// The refusal names the first rule that refused, with its wait.
func (t *throttler) reserve(ctx context.Context, rules ...throttleRule) (*reservation, error) {
	res := &reservation{t: t}
	if t.disabled {
		return res, nil
	}
	var active []throttleRule
	for _, rule := range rules {
		if rule.limit.on() && rule.subject != "" {
			active = append(active, rule)
		}
	}
	previous := make([]int64, len(active))
	for i, rule := range active {
		window, err := t.read(ctx, rule)
		if err != nil {
			t.warn(err)
			continue
		}
		previous[i] = window.previous
		if wait := window.wait(window.current + 1); wait > 0 {
			return nil, t.refuse(rule, wait)
		}
	}
	for i, rule := range active {
		key, wait, err := t.take(ctx, rule, previous[i])
		if err != nil {
			t.warn(err)
			continue
		}
		if wait > 0 {
			res.refund()
			return nil, t.refuse(rule, wait)
		}
		res.keys = append(res.keys, key)
	}
	return res, nil
}

func (t *throttler) refuse(rule throttleRule, wait time.Duration) *ThrottleError {
	t.logRefusal(rule.name)
	return &ThrottleError{Rule: rule.name, RetryAfter: wait}
}

// slidingWindow is a rule's two buckets at an instant: the current one's
// count, the previous one's, and how much of the current one has passed.
type slidingWindow struct {
	limit             ThrottleLimit
	current, previous int64
	elapsed           float64
}

// wait is how long until an attempt that makes the current bucket count
// fits the limit; 0 when it fits now. The window's count is the current
// bucket plus the previous one weighted by how much of it the window
// still covers.
func (w slidingWindow) wait(count int64) time.Duration {
	previous, limit := float64(max(w.previous, 0)), float64(w.limit.Limit)
	if previous*(1-w.elapsed)+float64(count) <= limit {
		return 0
	}
	return throttleWait(previous, float64(count-1), limit, w.elapsed, w.limit.Window)
}

// buckets names rule's current and previous bucket keys at now, and how
// much of the current one has passed.
func (t *throttler) buckets(rule throttleRule) (current, previous string, elapsed float64) {
	window := rule.limit.Window
	now := t.now()
	index := now.UnixNano() / int64(window)
	base := throttleKeyBase(rule)
	elapsed = float64(now.UnixNano()-index*int64(window)) / float64(window)
	return base + strconv.FormatInt(index, 10), base + strconv.FormatInt(index-1, 10), elapsed
}

// read loads rule's window without writing.
func (t *throttler) read(ctx context.Context, rule throttleRule) (slidingWindow, error) {
	currentKey, previousKey, elapsed := t.buckets(rule)
	w := slidingWindow{limit: rule.limit, elapsed: elapsed}
	store := t.cache()
	if _, err := store.Get(ctx, currentKey, &w.current); err != nil {
		return slidingWindow{}, err
	}
	if _, err := store.Get(ctx, previousKey, &w.previous); err != nil {
		return slidingWindow{}, err
	}
	return w, nil
}

// take counts one attempt for rule in the current bucket, with previous
// (read just before) for the window's estimate. Over the limit, it takes
// the attempt back and returns the wait.
func (t *throttler) take(ctx context.Context, rule throttleRule, previous int64) (reservedKey, time.Duration, error) {
	currentKey, _, elapsed := t.buckets(rule)
	current := reservedKey{rule: rule.name, key: currentKey, ttl: 2 * rule.limit.Window}
	count, err := cache.IncrementTTL(ctx, t.cache(), current.key, 1, current.ttl)
	if err != nil {
		return reservedKey{}, 0, err
	}
	// This attempt is in the count whatever the bucket held (a refund of a
	// counter recreated by something else, say): never less than one.
	count = max(count, 1)
	w := slidingWindow{limit: rule.limit, previous: previous, elapsed: elapsed}
	if wait := w.wait(count); wait > 0 {
		t.refundKey(current)
		return reservedKey{}, wait, nil
	}
	return current, 0, nil
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
	current, previous, _ := t.buckets(rule)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), throttleRefundTimeout)
	defer cancel()
	if err := t.cache().Delete(ctx, current, previous); err != nil {
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
	// Only a bucket that still exists: one cleared (a login's success, a
	// reset), expired or evicted meanwhile has nothing to give back, and a
	// decrement that created it would leave a negative count.
	if _, _, err := cache.DecrementExisting(ctx, t.cache(), key.key, 1); err != nil {
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

// loginRules are the rules a login attempt for email counts in: the
// address first (a subject the attacker cannot vary per request), then the
// address and email, then the email, unless this address is a known client
// of it (markKnown).
func (s *Service) loginRules(ctx context.Context, email string) (pair throttleRule, all []throttleRule) {
	p, address := s.throttle.policy, requestAddress(ctx)
	if email == "" {
		email = "\x00" // still one subject: the empty email is typed too
	}
	pair = throttleRule{name: "LoginPerAccountAddress", limit: p.LoginPerAccountAddress}
	if address != "" {
		pair.subject = address + "\x00" + email
	}
	all = []throttleRule{{name: "LoginPerAddress", limit: p.LoginPerAddress, subject: address}, pair}
	if !s.throttle.known(ctx, pair) {
		all = append(all, accountLoginRule(p, email))
	}
	return pair, all
}

func accountLoginRule(p ThrottlePolicy, email string) throttleRule {
	return throttleRule{name: "LoginPerAccount", limit: p.LoginPerAccount, subject: email}
}

// knownKey names the mark a successful login leaves for its address and
// email (pair's subject), hashed like the counters.
func knownKey(pair throttleRule) string {
	return throttleKeyBase(throttleRule{name: "KnownClient", subject: pair.subject}) + "known"
}

// known reports whether pair's address signed in to its email within
// KnownClientTTL: it may then try past LoginPerAccount, which anyone can
// fill. Only the password mints the mark, so it is no oracle.
func (t *throttler) known(ctx context.Context, pair throttleRule) bool {
	if t.disabled || t.policy.KnownClientTTL <= 0 || pair.subject == "" {
		return false
	}
	var mark bool
	found, err := t.cache().Get(ctx, knownKey(pair), &mark)
	if err != nil {
		t.warn(err)
		return false
	}
	return found && mark
}

// markKnown records a successful login of pair's address to its email.
func (t *throttler) markKnown(ctx context.Context, pair throttleRule) {
	if t.disabled || t.policy.KnownClientTTL <= 0 || pair.subject == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), throttleRefundTimeout)
	defer cancel()
	if err := t.cache().Set(ctx, knownKey(pair), true, t.policy.KnownClientTTL); err != nil {
		t.warn(err)
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
