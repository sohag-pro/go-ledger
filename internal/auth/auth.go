// Package auth resolves a bearer API key to a domain.APIKey, backed by a
// short-TTL in-memory cache so the request hot path does not hit the
// database on every call. See docs/adr/012-api-authentication-and-hardening.md.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// defaultTTL is used when NewResolver is given a non-positive ttl.
const defaultTTL = 30 * time.Second

// touchThrottle bounds how often a successful Resolve persists last_used_at
// for the same key (Task 2.2): touching it is best-effort and throttled so
// that a hot key does not turn into a write on every single request. A key
// used more than once within this window only gets one UPDATE.
const touchThrottle = 60 * time.Second

// touchTimeout bounds the asynchronous last_used_at write itself so a slow or
// wedged database does not leak goroutines across requests indefinitely.
const touchTimeout = 5 * time.Second

// bearerScheme is the auth scheme name stripped case-insensitively from the
// Authorization header value. It is optional: a bare token also resolves.
const bearerScheme = "Bearer"

// ErrUnauthorized is returned for any credential that cannot be resolved to a
// live key: an empty or garbage token, or a hash the lookup does not
// recognize. Callers map it to HTTP 401. It deliberately does not distinguish
// "missing" from "unknown" from "revoked": telling a caller which one it hit
// would help an attacker enumerate valid keys.
var ErrUnauthorized = errors.New("auth: unauthorized")

// keyLookup is the persistence dependency Resolver needs. The postgres
// repository satisfies it; tests use a fake.
type keyLookup interface {
	GetAPIKeyByHash(ctx context.Context, hash string) (domain.APIKey, error)

	// TouchAPIKeyLastUsed persists a key's last_used_at (Task 2.2). Resolve
	// calls it asynchronously and throttled (touchThrottle), and ignores its
	// error beyond a debug log: a failed touch must never fail the request
	// that triggered it.
	TouchAPIKeyLastUsed(ctx context.Context, id string, when time.Time) error
}

// cacheEntry is a resolved key plus when it stops being trusted without a
// re-fetch.
type cacheEntry struct {
	key       domain.APIKey
	expiresAt time.Time
}

// Resolver resolves a bearer token to a domain.APIKey, caching hits by key
// hash for ttl. It never caches the plaintext token, only its SHA-256 hash
// (via domain.HashAPIKey) and the resolved key.
//
// Misses (unknown hash, or a lookup that returns domain.ErrAPIKeyNotFound)
// are not cached: this keeps a mistyped or revoked-and-retried key from
// getting a free pass for the TTL window, at the cost of a database round
// trip on every failed attempt. Given failed attempts are expected to be
// rare compared to legitimate traffic, that trade favors correctness of
// revocation over hot-path cost for the failure case.
type Resolver struct {
	lookup keyLookup
	ttl    time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry

	// now is injected so expiry is testable without real sleeps. Defaults to
	// time.Now; tests in this package may overwrite it directly on a
	// Resolver they constructed themselves (same-package white-box access).
	now func() time.Time
}

// NewResolver builds a Resolver backed by lookup. A non-positive ttl falls
// back to defaultTTL.
func NewResolver(lookup keyLookup, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Resolver{
		lookup: lookup,
		ttl:    ttl,
		cache:  make(map[string]cacheEntry),
		now:    time.Now,
	}
}

// Resolve strips an optional "Bearer " prefix from bearer, hashes the
// remaining token, and resolves it to a domain.APIKey, preferring the cache.
// It returns ErrUnauthorized for an empty token, a hash with no live key, or a
// key whose ExpiresAt has passed (Task 2.2: an expired key is a dead
// credential, the same class as unknown or revoked, so it gets the same 401
// and reveals nothing more). It returns a *domain.TenantNotActiveError
// (matched via errors.Is(err, domain.ErrTenantNotActive)) when the key is
// valid but its tenant is suspended or closed (ADR-015, Task 2.1): the
// credential itself is fine, so this is a distinct failure from
// ErrUnauthorized, and a transport layer should map it to 403 /
// PermissionDenied rather than 401 / Unauthenticated. Scope enforcement is
// not done here: it depends on which operation is being called, so it lives
// in the huma middleware and the gRPC interceptor, which call CheckScope
// themselves after a successful Resolve.
//
// On a successful resolve (past both gates), Resolve best-effort touches the
// key's last_used_at, throttled to at most once per touchThrottle: see
// maybeTouchLastUsed.
func (r *Resolver) Resolve(ctx context.Context, bearer string) (domain.APIKey, error) {
	token := strings.TrimSpace(bearer)
	if isBearerScheme(token) {
		token = strings.TrimSpace(token[len(bearerScheme):])
	}
	if token == "" {
		return domain.APIKey{}, ErrUnauthorized
	}

	hash := domain.HashAPIKey(token)
	now := r.now

	if key, ok := r.cacheGet(hash, now()); ok {
		return r.finishResolve(ctx, hash, key, now())
	}

	key, err := r.lookup.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			return domain.APIKey{}, ErrUnauthorized
		}
		return domain.APIKey{}, fmt.Errorf("auth: resolve key: %w", err)
	}

	// Cache the resolved key (and the tenant status alongside it) regardless
	// of whether the gates below pass: a cached "suspended" or "expired"
	// entry is what makes the cache-hit path above re-check status on every
	// call instead of only on a cold miss, and a re-fetch after the entry's
	// TTL expires is what picks up a tenant getting reactivated (or newly
	// gated) within one AUTH_CACHE_TTL window, with no extra database round
	// trip beyond the key lookup itself. cachePut itself caps the entry's
	// expiry at the key's own ExpiresAt, so a key is never cached past the
	// point it stops being valid.
	r.cachePut(hash, key, now())
	return r.finishResolve(ctx, hash, key, now())
}

// finishResolve applies the expiry gate (Task 2.2) and the tenant-status gate
// (Task 2.1) to key, in that order: an expired key is rejected before its
// tenant's status is even considered, since a dead credential should not
// reveal anything about the tenant it used to belong to. It is the single
// place Resolve checks these gates, run identically on a cache hit and a
// fresh lookup. Only on success does it consider touching last_used_at.
func (r *Resolver) finishResolve(ctx context.Context, hash string, key domain.APIKey, now time.Time) (domain.APIKey, error) {
	if key.IsExpired(now) {
		return domain.APIKey{}, fmt.Errorf("%w: %w", ErrUnauthorized, domain.ErrAPIKeyExpired)
	}
	gated, err := gateTenantStatus(key)
	if err != nil {
		return gated, err
	}
	r.maybeTouchLastUsed(ctx, hash, key, now)
	return gated, nil
}

// gateTenantStatus returns key unchanged if its tenant is active, or a
// *domain.TenantNotActiveError if not.
func gateTenantStatus(key domain.APIKey) (domain.APIKey, error) {
	if key.TenantStatus != domain.TenantActive {
		return domain.APIKey{}, &domain.TenantNotActiveError{TenantID: key.TenantID, Status: key.TenantStatus}
	}
	return key, nil
}

// maybeTouchLastUsed fires an asynchronous, best-effort TouchAPIKeyLastUsed
// for key if it has not been touched within touchThrottle. The
// due-and-mark-done check runs atomically under the cache lock (shouldTouch)
// so concurrent Resolve calls for the same key only fire one touch per
// window, but the database write itself happens in a detached goroutine, off
// the request path and never holding the cache lock.
func (r *Resolver) maybeTouchLastUsed(ctx context.Context, hash string, key domain.APIKey, now time.Time) {
	if !r.shouldTouch(hash, now) {
		return
	}
	go func(id string, when time.Time) {
		touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), touchTimeout)
		defer cancel()
		if err := r.lookup.TouchAPIKeyLastUsed(touchCtx, id, when); err != nil {
			slog.Default().Debug("auth: touch api key last_used_at failed",
				slog.String("key_id", id), slog.String("error", err.Error()))
		}
	}(key.ID, now)
}

// shouldTouch reports whether hash's cache entry is due for a last_used_at
// touch (its LastUsedAt is nil or older than touchThrottle), and if so marks
// it touched as of now in the same locked step: this is what makes the check
// and the mark atomic, so two goroutines racing to resolve the same key
// within one throttle window cannot both decide a touch is due.
func (r *Resolver) shouldTouch(hash string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.cache[hash]
	if !ok {
		return false
	}
	if entry.key.LastUsedAt != nil && now.Sub(*entry.key.LastUsedAt) < touchThrottle {
		return false
	}
	entry.key.LastUsedAt = &now
	r.cache[hash] = entry
	return true
}

// isBearerScheme reports whether token starts with the case-insensitive
// "Bearer" scheme name followed by a boundary (whitespace or end of string),
// so a bare token that merely happens to start with those letters (unlikely
// given the "glk_" key prefix, but not impossible) is not mistaken for one.
func isBearerScheme(token string) bool {
	if len(token) < len(bearerScheme) || !strings.EqualFold(token[:len(bearerScheme)], bearerScheme) {
		return false
	}
	if len(token) == len(bearerScheme) {
		return true
	}
	next := token[len(bearerScheme)]
	return next == ' ' || next == '\t'
}

func (r *Resolver) cacheGet(hash string, now time.Time) (domain.APIKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.cache[hash]
	if !ok || !now.Before(entry.expiresAt) {
		return domain.APIKey{}, false
	}
	return entry.key, true
}

// cachePut caches key under hash until now+ttl, capped at key.ExpiresAt when
// that is sooner (Task 2.2): a key is never trusted from cache past the point
// it expires, however long AUTH_CACHE_TTL is set to. If key is already
// expired at the time it is cached, the cap puts expiresAt in the past, so
// the entry is immediately unusable on the very next cacheGet, the same
// effect as not caching it at all.
func (r *Resolver) cachePut(hash string, key domain.APIKey, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	expiresAt := now.Add(r.ttl)
	if key.ExpiresAt != nil && key.ExpiresAt.Before(expiresAt) {
		expiresAt = *key.ExpiresAt
	}
	r.cache[hash] = cacheEntry{key: key, expiresAt: expiresAt}
}
