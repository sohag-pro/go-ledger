package auth

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// builtinDefaultRPM is used when NewLimiter is given a non-positive default
// and, in turn, for any key whose own RateLimitRPM is nil or non-positive.
// It is deliberately generous for a normal key; the demo key and any
// load-test key set their own rate_limit_rpm (see
// docs/adr/012-api-authentication-and-hardening.md, "Per-key rate limiting").
const builtinDefaultRPM = 120

// Limiter is a per-API-key token bucket rate limiter. Each distinct key id
// gets its own *rate.Limiter, created lazily on first use and kept for the
// life of the process; there is no eviction. At go-ledger's key scale (one
// entry per issued key, not per request or per tenant transaction) an
// unbounded map is acceptable. If key churn ever becomes large enough for
// this to matter, the fix is a size cap or an idle-TTL sweep, not a redesign.
type Limiter struct {
	mu         sync.Mutex
	buckets    map[string]*rate.Limiter
	defaultRPM int
}

// NewLimiter returns a Limiter that allows defaultRPM requests per minute for
// any key that does not carry its own override.
//
// defaultRPM is clamped to builtinDefaultRPM when it is not a usable limit
// (zero or negative): a zero limit would lock every unpinned key out
// entirely (rate.Limit(0) never allows a request) and a negative burst
// passed to rate.NewLimiter panics. The same clamp is applied per key below,
// since RateLimitRPM is an untrusted *int coming out of the database and a
// bad row (0, negative, or some future overflow) must degrade to the
// default rather than take a key's traffic to zero or crash the process.
func NewLimiter(defaultRPM int) *Limiter {
	if defaultRPM <= 0 {
		defaultRPM = builtinDefaultRPM
	}
	return &Limiter{
		buckets:    make(map[string]*rate.Limiter),
		defaultRPM: defaultRPM,
	}
}

// rpmFor returns the requests-per-minute limit that applies to key: its own
// RateLimitRPM if set and positive, otherwise the limiter's default. This is
// the single place that narrows the untrusted *int from domain.APIKey, so
// every caller below goes through the same guard.
func (l *Limiter) rpmFor(key domain.APIKey) int {
	if key.RateLimitRPM != nil && *key.RateLimitRPM > 0 {
		return *key.RateLimitRPM
	}
	return l.defaultRPM
}

// limiterFor returns key's bucket, creating it on first use. The limit is
// expressed as rpm/60 tokens per second (rate.Limit takes a per-second rate),
// with burst set to the full per-minute number: a key allowed 60 rpm can
// spend its whole minute's budget in one go if it has been idle, then must
// wait for the bucket to refill at one token per second. A smaller burst
// would reject perfectly legitimate bursty callers (a batch job that wakes
// up once a minute) even though they are within budget over any one-minute
// window; a larger burst would let a key spend more than one window's worth
// at once. Burst equal to the per-minute rate is the natural middle ground
// for an RPM-denominated limit.
//
// Once created, a key's bucket keeps the rpm it was created with even if the
// underlying APIKey.RateLimitRPM changes later (there is no key lookup on
// the hot path here, only the id used as the map key); a changed limit takes
// effect the next time the process restarts or the key is evicted. This
// mirrors the auth cache's own lag-behind-revocation tradeoff (see
// middleware.go / docs/adr/012) and is acceptable for the same reason: it is
// bounded and known, not unbounded staleness.
func (l *Limiter) limiterFor(key domain.APIKey) *rate.Limiter {
	rpm := l.rpmFor(key)

	l.mu.Lock()
	defer l.mu.Unlock()

	lim, ok := l.buckets[key.ID]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(rpm)/60, rpm)
		l.buckets[key.ID] = lim
	}
	return lim
}

// Allow reports whether a request for key may proceed right now, consuming
// one token from that key's bucket if so. It is the primitive both
// middlewares below are built on, and is exported directly so tests (and any
// non-HTTP caller, e.g. a gRPC interceptor added later) can drive it without
// going through net/http.
func (l *Limiter) Allow(key domain.APIKey) bool {
	return l.limiterFor(key).Allow()
}

// retryAfterSeconds returns the value to send back in a 429's Retry-After
// header for a key limited to rpm requests per minute: the time for one
// token to refill at that key's steady drain rate, rounded up and floored at
// one second. This is an approximation (the bucket may already hold a
// partial token from before the request that got rejected), not an exact
// wait time from the limiter's internal reservation clock, but it is a
// caller-friendly, always-safe-to-retry-after value and does not require
// exposing the limiter's internal state.
func retryAfterSeconds(rpm int) int {
	if rpm <= 0 {
		rpm = builtinDefaultRPM
	}
	secs := 60 / rpm
	if secs < 1 {
		secs = 1
	}
	return secs
}

// HumaMiddleware returns a huma middleware enforcing l against the API key
// already resolved into the request context by auth.HumaMiddleware. It must
// be registered after (i.e. it runs downstream of) the auth middleware, so
// that KeyFromContext has something to find; on go-ledger's /v1 routes auth
// always runs first, per docs/adr/012.
//
// If no key is present in the context, this middleware allows the request
// through rather than failing closed with a 500. Rate limiting is
// defense-in-depth on top of authentication, not itself an access control:
// auth.HumaMiddleware is the component responsible for rejecting a request
// that has no valid key (401), and it always sets a key on any path it lets
// through. A missing key here means either the request reached an
// unauthenticated route (health, openapi, the console) that this middleware
// was, by misconfiguration, also applied to, or an ordering bug in main.go
// (Task 10's wiring). Neither case should be turned into an outage for
// unrelated traffic by a component whose only job is throttling.
func (l *Limiter) HumaMiddleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		key, ok := KeyFromContext(ctx.Context())
		if !ok {
			next(ctx)
			return
		}

		if !l.Allow(key) {
			ctx.SetHeader("Retry-After", strconv.Itoa(retryAfterSeconds(l.rpmFor(key))))
			_ = huma.WriteErr(api, ctx, http.StatusTooManyRequests, "")
			return
		}

		next(ctx)
	}
}

// Middleware returns a net/http middleware equivalent to HumaMiddleware, for
// the same reason auth.Middleware exists alongside auth.HumaMiddleware: a
// chi-level fallback in case a route composition ever needs it outside huma.
// It is not wired into cmd/server; that wiring, and the configured default
// RPM, are Task 10's responsibility.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := KeyFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		if !l.Allow(key) {
			writeTooManyRequests(w, retryAfterSeconds(l.rpmFor(key)))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// writeTooManyRequests writes the same problem+json shape as auth's own
// writeUnauthorized, for the net/http fallback path which has no huma.API to
// negotiate content through.
func writeTooManyRequests(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"status":429,"title":"Too Many Requests"}`))
}
