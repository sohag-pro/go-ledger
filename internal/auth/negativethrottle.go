package auth

import (
	"sync"
	"time"
)

// DefaultNegativeThrottleMaxFailures and DefaultNegativeThrottleWindow are
// used when NewNegativeThrottle is given a non-positive value for either
// argument. 20 failures per minute is generous enough that a legitimate
// caller retrying a key it just rotated, or a shared NAT/office IP with a
// handful of independently misconfigured clients, does not get cut off, while
// still capping a garbage-key flood to at most 20 database lookups per IP per
// minute (see NegativeThrottle's own doc comment for the vector this closes).
const (
	DefaultNegativeThrottleMaxFailures = 20
	DefaultNegativeThrottleWindow      = time.Minute
)

// negativeThrottleMaxTrackedIPs bounds the number of distinct IPs
// NegativeThrottle ever holds an entry for at once. Without this bound, an
// attacker who cannot exhaust the database pool directly (each IP is capped
// at maxFailures database lookups) could instead spray the flood across many
// source IPs (a botnet, a rotating pool of proxies) and exhaust process
// memory one map entry at a time instead: the throttle meant to protect the
// pool would become a memory-exhaustion vector of its own. 10000 tracked IPs
// at well under 100 bytes each is a small, fixed memory cost regardless of
// how many distinct attacking IPs show up.
const negativeThrottleMaxTrackedIPs = 10_000

// NegativeThrottle limits failed authentications per client IP so a
// garbage-key flood cannot exhaust the connection pool with one DB lookup
// per bad key. auth.Resolver deliberately does not cache a miss (an unknown,
// expired, or revoked key hash): see Resolver's own doc comment for why that
// trade favors correctness of revocation over hot-path cost. The cost of
// that choice is that every failed credential is a full database round trip;
// NegativeThrottle is what keeps a flood of failed credentials from one
// source from turning into a flood of database round trips: past
// maxFailures failures within window, Allowed returns false and the caller
// (HumaMiddleware, Middleware) rejects the request BEFORE calling the
// resolver at all.
//
// Entries decay per IP: a window that has elapsed since it started resets on
// the next Allowed or RecordFailure call for that IP, so a throttled IP is
// not blocked forever, only for the rest of its current window. The map
// itself is bounded at negativeThrottleMaxTrackedIPs (see that constant's
// doc comment); once at capacity, RecordFailure evicts the single oldest
// entry to make room rather than growing further.
type NegativeThrottle struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	entries     map[string]*ipFailureWindow

	// now is injected so window expiry is testable without real sleeps.
	// Defaults to time.Now; tests in this package may overwrite it directly
	// on a NegativeThrottle they constructed themselves (same-package
	// white-box access), mirroring Resolver's own now field.
	now func() time.Time
}

// ipFailureWindow is one IP's failure count for its current window.
type ipFailureWindow struct {
	count      int
	windowFrom time.Time
}

// NewNegativeThrottle builds a NegativeThrottle allowing at most maxFailures
// failed lookups per IP within window before Allowed starts returning false
// for that IP. A non-positive maxFailures or window falls back to
// DefaultNegativeThrottleMaxFailures / DefaultNegativeThrottleWindow
// respectively, the same non-positive-falls-back-to-a-sane-default shape as
// auth.NewResolver's ttl argument.
func NewNegativeThrottle(maxFailures int, window time.Duration) *NegativeThrottle {
	if maxFailures <= 0 {
		maxFailures = DefaultNegativeThrottleMaxFailures
	}
	if window <= 0 {
		window = DefaultNegativeThrottleWindow
	}
	return &NegativeThrottle{
		maxFailures: maxFailures,
		window:      window,
		entries:     make(map[string]*ipFailureWindow),
		now:         time.Now,
	}
}

// Allowed reports whether ip is still under its failure budget for its
// current window. An IP with no entry, or whose entry's window has already
// elapsed, is allowed: a stale entry is left in place for RecordFailure to
// reset (or evict) rather than deleted here, since Allowed is called far
// more often than RecordFailure (every request vs. only failed ones) and
// should not pay for a map write on the common path.
func (t *NegativeThrottle) Allowed(ip string) bool {
	if ip == "" {
		// No IP to key on (should not happen given clientIP's fallback to
		// RemoteAddr, but defensive): fail open rather than block every
		// caller globally under one empty-string bucket.
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.entries[ip]
	if !ok {
		return true
	}
	if t.now().Sub(entry.windowFrom) >= t.window {
		return true
	}
	return entry.count < t.maxFailures
}

// RecordFailure records one failed authentication attempt from ip, called
// after a resolver.Resolve that failed with ErrUnauthorized (an unknown,
// expired, or revoked key): see HumaMiddleware and Middleware. A failure
// whose IP's previous window has already elapsed starts a fresh window
// rather than accumulating against the old one, which is what makes the
// block temporary (the rest of the current window), not permanent.
func (t *NegativeThrottle) RecordFailure(ip string) {
	if ip == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.evictStaleLocked(now)

	if entry, ok := t.entries[ip]; ok && now.Sub(entry.windowFrom) < t.window {
		entry.count++
		return
	}

	if len(t.entries) >= negativeThrottleMaxTrackedIPs {
		t.evictOldestLocked()
	}
	t.entries[ip] = &ipFailureWindow{count: 1, windowFrom: now}
}

// RecordSuccess clears ip's failure entry, called after a resolver.Resolve
// that succeeded: a client behind ip that has just authenticated correctly
// gets its failure budget back immediately rather than waiting out the rest
// of the window, so one bad key typed earlier in a session does not linger
// as reduced headroom after the caller corrects it.
func (t *NegativeThrottle) RecordSuccess(ip string) {
	if ip == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, ip)
}

// evictStaleLocked drops every entry whose window has already elapsed. It is
// called with t.mu already held, from RecordFailure only: Allowed (the much
// hotter path) tolerates a stale entry lingering until the next write instead
// of paying for a map delete on every request.
func (t *NegativeThrottle) evictStaleLocked(now time.Time) {
	for ip, entry := range t.entries {
		if now.Sub(entry.windowFrom) >= t.window {
			delete(t.entries, ip)
		}
	}
}

// evictOldestLocked drops the single entry with the earliest windowFrom, so
// RecordFailure can make room for a new IP once the map is at
// negativeThrottleMaxTrackedIPs capacity and evictStaleLocked found nothing
// stale to reclaim (a spray of many distinct IPs, each still within its own
// window). This trades a little precision, an evicted IP's budget resets
// early, for the hard cap that keeps the throttle itself from becoming an
// unbounded memory sink. Called with t.mu already held.
func (t *NegativeThrottle) evictOldestLocked() {
	var oldestIP string
	var oldestAt time.Time
	first := true
	for ip, entry := range t.entries {
		if first || entry.windowFrom.Before(oldestAt) {
			oldestIP, oldestAt, first = ip, entry.windowFrom, false
		}
	}
	if oldestIP != "" {
		delete(t.entries, oldestIP)
	}
}
