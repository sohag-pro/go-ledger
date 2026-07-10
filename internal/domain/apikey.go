package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"
)

const apiKeyPrefix = "glk_"

// Scope is a capability an API key carries (Task 2.2, audit A3.2/A2.3). A key
// resolves to zero or more scopes, enforced at the transport boundary (the
// huma middleware and the gRPC interceptor), not in the domain model itself.
type Scope string

const (
	// ScopeRead lets a key call read-only operations: safe HTTP methods (GET,
	// HEAD, OPTIONS) and read RPCs (Get*, List*, the audit read/verify RPCs).
	ScopeRead Scope = "read"
	// ScopePost lets a key call mutating operations: HTTP POST/PUT/PATCH/DELETE
	// and any RPC that writes (PostTransaction, Convert, ...).
	ScopePost Scope = "post"
	// ScopeAdmin lets a key call the future admin surface (anything under
	// /v1/admin/). ScopeAdmin is a superset of ScopeRead and ScopePost: a key
	// carrying it satisfies any required scope without also needing "read" and
	// "post" listed explicitly. See APIKey.HasScope.
	ScopeAdmin Scope = "admin"
)

// Valid reports whether s is one of the three defined scopes. The api_keys
// table's api_keys_scopes_valid CHECK constraint (migration 0012) enforces
// the same set at the schema level.
func (s Scope) Valid() bool {
	switch s {
	case ScopeRead, ScopePost, ScopeAdmin:
		return true
	default:
		return false
	}
}

// APIKey is a resolved credential: which tenant it acts as, what it is
// allowed to do, and its optional per-key rate limit (nil means the server
// default).
//
// TenantStatus is the status of TenantID as of the resolving lookup (Task
// 2.1, ADR-015): the auth resolver gates on it so a suspended or closed
// tenant's key stops working within one cache TTL, with no extra round trip
// beyond the key lookup itself. A lookup that does not join the tenants
// table (a test double that predates tenants, for instance) leaves this at
// its zero value, which is not a valid TenantStatus and is treated as not
// active.
//
// Scopes, ExpiresAt, and LastUsedAt are the Task 2.2 lifecycle fields.
// ExpiresAt is nil for a key that never expires; LastUsedAt is nil for a key
// that has never been used (or whose last use has not yet been persisted,
// since the auth resolver touches it best-effort and throttled rather than on
// every request).
type APIKey struct {
	ID           string
	TenantID     string
	Name         string
	RateLimitRPM *int
	TenantStatus TenantStatus
	Scopes       []Scope
	ExpiresAt    *time.Time
	LastUsedAt   *time.Time
}

// HasScope reports whether k is allowed to perform an operation requiring
// required. ScopeAdmin is a superset (the chosen model, documented on
// ScopeAdmin itself): a key carrying ScopeAdmin satisfies any required scope,
// so an admin key does not also need "read" and "post" listed explicitly.
func (k APIKey) HasScope(required Scope) bool {
	for _, s := range k.Scopes {
		if s == ScopeAdmin || s == required {
			return true
		}
	}
	return false
}

// IsExpired reports whether k's expiry, if any, is at or before now. A nil
// ExpiresAt means the key never expires, so IsExpired is always false in that
// case.
func (k APIKey) IsExpired(now time.Time) bool {
	if k.ExpiresAt == nil {
		return false
	}
	return !now.Before(*k.ExpiresAt)
}

// GenerateAPIKey returns a new random key plaintext ("glk_<base64url>") and its
// SHA-256 hex hash. Only the hash is ever stored.
func GenerateAPIKey() (plaintext, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	plaintext = apiKeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, HashAPIKey(plaintext), nil
}

// HashAPIKey returns the SHA-256 hex of a key plaintext. Deterministic so a
// presented key can be looked up by hash.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// InsufficientScopeError is returned when an API key's scopes do not include
// the one an operation requires. It wraps ErrInsufficientScope so callers can
// match with errors.Is(err, ErrInsufficientScope) without caring which scope
// was required, mirroring TenantNotActiveError (Task 2.1).
type InsufficientScopeError struct {
	Required Scope
}

// Error implements the error interface.
func (e *InsufficientScopeError) Error() string {
	return "api key missing required scope: " + string(e.Required)
}

// Unwrap lets errors.Is(err, ErrInsufficientScope) match regardless of which
// scope was required.
func (e *InsufficientScopeError) Unwrap() error { return ErrInsufficientScope }

// Reason returns the caller-facing explanation for a 403 / PermissionDenied
// response: naming the missing scope does not help an attacker enumerate
// keys, since the credential was already known to be valid to get this far.
func (e *InsufficientScopeError) Reason() string {
	return "missing required scope: " + string(e.Required)
}
