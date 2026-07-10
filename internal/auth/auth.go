// Package auth resolves a bearer API key to a domain.APIKey, backed by a
// short-TTL in-memory cache so the request hot path does not hit the
// database on every call. See docs/adr/012-api-authentication-and-hardening.md.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// defaultTTL is used when NewResolver is given a non-positive ttl.
const defaultTTL = 30 * time.Second

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
// It returns ErrUnauthorized for an empty token or a hash with no live key,
// and a *domain.TenantNotActiveError (matched via errors.Is(err,
// domain.ErrTenantNotActive)) when the key is valid but its tenant is
// suspended or closed (ADR-015, Task 2.1): the credential itself is fine, so
// this is a distinct failure from ErrUnauthorized, and a transport layer
// should map it to 403 / PermissionDenied rather than 401 / Unauthenticated.
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
		return gateTenantStatus(key)
	}

	key, err := r.lookup.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			return domain.APIKey{}, ErrUnauthorized
		}
		return domain.APIKey{}, fmt.Errorf("auth: resolve key: %w", err)
	}

	// Cache the resolved key (and the tenant status alongside it) regardless
	// of whether the gate below passes: a cached "suspended" entry is what
	// makes the cache-hit path above re-check status on every call instead of
	// only on a cold miss, and a re-fetch after the entry's TTL expires is
	// what picks up a tenant getting reactivated (or newly gated) within one
	// AUTH_CACHE_TTL window, with no extra database round trip beyond the key
	// lookup itself.
	r.cachePut(hash, key, now())
	return gateTenantStatus(key)
}

// gateTenantStatus returns key unchanged if its tenant is active, or a
// *domain.TenantNotActiveError if not. It is the single place Resolve checks
// tenant status, run identically on a cache hit and a fresh lookup, so a
// suspended or closed tenant is rejected on every call, not just the first
// one that misses the cache.
func gateTenantStatus(key domain.APIKey) (domain.APIKey, error) {
	if key.TenantStatus != domain.TenantActive {
		return domain.APIKey{}, &domain.TenantNotActiveError{TenantID: key.TenantID, Status: key.TenantStatus}
	}
	return key, nil
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

func (r *Resolver) cachePut(hash string, key domain.APIKey, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cache[hash] = cacheEntry{key: key, expiresAt: now.Add(r.ttl)}
}
