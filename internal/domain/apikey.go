package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const apiKeyPrefix = "glk_"

// APIKey is a resolved credential: which tenant it acts as and its optional
// per-key rate limit (nil means the server default).
//
// TenantStatus is the status of TenantID as of the resolving lookup (Task
// 2.1, ADR-015): the auth resolver gates on it so a suspended or closed
// tenant's key stops working within one cache TTL, with no extra round trip
// beyond the key lookup itself. A lookup that does not join the tenants
// table (a test double that predates tenants, for instance) leaves this at
// its zero value, which is not a valid TenantStatus and is treated as not
// active.
type APIKey struct {
	ID           string
	TenantID     string
	Name         string
	RateLimitRPM *int
	TenantStatus TenantStatus
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
