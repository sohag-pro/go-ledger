// Package crypto implements envelope encryption for posting descriptions
// (Task 6.2, audit A9.3): the technique that lets a data-subject erasure
// request destroy a tenant's ability to read its PII (free-text posting
// descriptions) WITHOUT mutating any money row or breaking the tamper-evident
// audit hash chain (ADR-012, ADR-017).
//
// The scheme is standard envelope encryption, two layers deep:
//
//   - A single MASTER KEY, 32 bytes, supplied by the deployment via
//     LEDGER_MASTER_KEY (base64-encoded), never stored anywhere: it lives only
//     in process memory and whatever secret store injects the environment
//     variable.
//   - A per-tenant DATA ENCRYPTION KEY (DEK), a random 32-byte key generated on
//     first use for a tenant, wrapped (AES-256-GCM) with the master key, and
//     stored as crypto_keys.wrapped_dek (migration 0027). The master key never
//     touches a posting description directly; it only ever wraps/unwraps DEKs.
//
// A posting description is encrypted with its tenant's DEK (AES-256-GCM, a
// fresh random nonce per call) and stored as a self-describing string,
// EncodingPrefix + base64(nonce || ciphertext), so a reader can always tell
// ciphertext from a legacy (pre-6.2) plaintext description: see Decrypt.
//
// SHREDDING a tenant's PII (the crypto-shredding technique, ADR-012's
// complement) is destroying its DEK: crypto_keys.wrapped_dek is set NULL and
// shredded_at is stamped (domain.Repository.ShredTenantCryptoKey). This
// package's Decrypt treats a shredded tenant's ciphertext as
// RedactedMarker, not an error, so reads keep working; it never needs the
// plaintext or the destroyed key again. Shredding never touches
// postings.description or audit_log.after: those bytes are unchanged, so the
// audit chain's row_hash (computed over those exact stored bytes,
// domain.ComputeAuditRowHash) still recomputes identically and the chain
// still verifies after a shred. This is the money-critical property the
// whole package exists to preserve; see AuditService.Verify's own doc
// comment for why it never decrypts.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// dekSize is the size, in bytes, of a tenant's Data Encryption Key and of the
// master key itself: AES-256, the same key size ADR-012's audit hashing and
// every other cryptographic primitive in this codebase already assumes for
// "256-bit".
const dekSize = 32

// EncodingPrefix marks a stored posting description as ciphertext produced by
// this package's "v1" scheme: Encrypt always prepends it, and Decrypt uses
// its presence to distinguish ciphertext from a legacy, pre-Task-6.2
// plaintext description (backward compatibility: a description stored before
// this feature existed carries no prefix and is returned as-is, unchanged).
// The "v1" tag leaves room for a future encoding change the same way
// domain.CurrentFingerprintScheme does for the idempotency fingerprint: a
// reader that knows only "v1" can still fail closed on an unrecognized future
// prefix instead of misinterpreting its bytes.
const EncodingPrefix = "enc:v1:"

// RedactedMarker is what Decrypt returns, with no error, for a ciphertext
// description belonging to a tenant whose key has been shredded (Task 6.2,
// audit A9.3): the whole point of crypto-shredding is that reads keep
// working after an erasure request, they just can no longer recover the
// original content.
const RedactedMarker = "[redacted: erased]"

// ErrTenantKeyShredded is returned by Encrypt when asked to encrypt new
// content for a tenant whose key has already been shredded. Shredding is
// documented as irreversible (see domain.Repository.ShredTenantCryptoKey):
// silently minting a fresh DEK here would let new data quietly become
// encryptable (and so, eventually, erasable) again for a tenant an operator
// deliberately cut off, undermining that guarantee. A tenant that needs to
// keep posting after an erasure request is an operational decision outside
// this package (see docs/ops/retention-and-erasure.md): typically the tenant
// is also suspended around the same time PII is shredded.
var ErrTenantKeyShredded = errors.New("crypto: tenant key has been shredded")

// KeyStore is the persistence port for per-tenant Data Encryption Keys
// (Task 6.2, audit A9.3): the crypto_keys table (migration 0027) behind it.
// internal/postgres.Repository implements this directly, alongside
// domain.Repository, so cmd/server can hand the same *postgres.Repository
// value to both NewCipher and postgres.NewRepository.
type KeyStore interface {
	// GetOrCreateWrappedDEK returns tenantID's wrapped DEK, atomically
	// creating one from candidateWrappedDEK if the tenant has none yet (an
	// INSERT ... ON CONFLICT DO UPDATE that forces RETURNING to yield
	// whichever row wins a race between concurrent callers, the same
	// "GetOrCreateClearingAccount" pattern internal/postgres/queries/accounts.sql
	// already established). candidateWrappedDEK is generated and wrapped by
	// the CALLER (Cipher), not the store, since only Cipher holds the master
	// key: the store's job is purely to persist bytes, never to know what
	// they mean.
	//
	// If the tenant's key has already been shredded, shredded is true and
	// wrappedDEK is nil, regardless of candidateWrappedDEK: a shredded
	// tenant's row is never revived by this call (see ErrTenantKeyShredded).
	GetOrCreateWrappedDEK(ctx context.Context, tenantID string, candidateWrappedDEK []byte) (wrappedDEK []byte, shredded bool, err error)

	// GetWrappedDEK returns tenantID's wrapped DEK for a decrypt, without
	// ever creating one: a description already carrying EncodingPrefix
	// implies a DEK was created at encrypt time, so a decrypt that finds none
	// (found is false) is a genuine inconsistency, not the normal
	// first-use case GetOrCreateWrappedDEK handles.
	GetWrappedDEK(ctx context.Context, tenantID string) (wrappedDEK []byte, shredded, found bool, err error)
}

// Cipher encrypts and decrypts posting descriptions with per-tenant data
// keys, wrapped by one deployment-wide master key (Task 6.2, audit A9.3). Use
// NewCipher to construct one; the zero value is not usable.
type Cipher struct {
	masterKey [dekSize]byte
	store     KeyStore
}

// NewCipher returns a Cipher backed by store, unwrapping/wrapping tenant DEKs
// with masterKeyB64: the base64 encoding of exactly 32 raw bytes. It fails
// fast (a non-nil error, before any encryption is ever attempted) if
// masterKeyB64 is not valid base64 or does not decode to exactly 32 bytes:
// see docs/ops/retention-and-erasure.md and cmd/server's own config loading
// for why a malformed master key must never be discovered lazily, on the
// first real request.
func NewCipher(masterKeyB64 string, store KeyStore) (*Cipher, error) {
	key, err := ParseMasterKey(masterKeyB64)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("crypto: NewCipher requires a non-nil KeyStore")
	}
	return &Cipher{masterKey: key, store: store}, nil
}

// ParseMasterKey decodes and validates a base64-encoded master key, failing
// fast on malformed input rather than truncating or padding it. Exported so
// cmd/server can validate LEDGER_MASTER_KEY at config-load time, before any
// other component is constructed, with the same error a later NewCipher call
// would produce.
func ParseMasterKey(masterKeyB64 string) ([dekSize]byte, error) {
	var out [dekSize]byte
	raw, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return out, fmt.Errorf("crypto: LEDGER_MASTER_KEY is not valid base64: %w", err)
	}
	if len(raw) != dekSize {
		return out, fmt.Errorf("crypto: LEDGER_MASTER_KEY must decode to %d bytes, got %d", dekSize, len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

// Encrypt returns plaintext encrypted under tenantID's data key, as a
// self-describing string a later Decrypt call can recognize (EncodingPrefix +
// base64(nonce || ciphertext)). An empty plaintext is returned unchanged
// ("" in, "" out): there is nothing to protect in an absent description, and
// encrypting it would turn "no description" into indistinguishable-from-real
// ciphertext on read back, breaking the "empty stays empty" contract
// internal/ledger's write path documents.
//
// It returns ErrTenantKeyShredded if tenantID's key has already been
// shredded: see ErrTenantKeyShredded's own doc comment for why this fails
// closed instead of silently minting a new key.
func (c *Cipher) Encrypt(ctx context.Context, tenantID, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	dek, shredded, err := c.tenantDEK(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if shredded {
		return "", ErrTenantKeyShredded
	}
	sealed, err := seal(dek, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("crypto: encrypt: %w", err)
	}
	return EncodingPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt returns stored's plaintext content, or a well-defined non-error
// substitute in two cases that are not failures:
//
//   - stored does not carry EncodingPrefix: it is legacy, pre-Task-6.2
//     plaintext (or already empty), returned unchanged. This is the backward
//     compatibility guarantee: every posting description written before this
//     feature existed reads back exactly as it always did.
//   - tenantID's key has been shredded: RedactedMarker is returned, not an
//     error, so a read of a crypto-shredded tenant's history keeps working
//     (Task 6.2, audit A9.3's core requirement) instead of failing every
//     GetTransaction/ListTransactions/Statement/audit-read call for that
//     tenant forever.
//
// Any other failure (no key material at all for a tenant whose data is
// nonetheless ciphertext, a corrupt or tampered ciphertext, an unwrap
// failure) is a genuine error: Decrypt only ever silently substitutes for the
// two well-understood, expected cases above.
func (c *Cipher) Decrypt(ctx context.Context, tenantID, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, EncodingPrefix) {
		return stored, nil
	}
	wrapped, shredded, found, err := c.store.GetWrappedDEK(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("crypto: look up tenant key: %w", err)
	}
	if !found {
		return "", fmt.Errorf("crypto: tenant %s has no key material, but stored value is ciphertext", tenantID)
	}
	if shredded {
		return RedactedMarker, nil
	}
	dek, err := unwrap(c.masterKey[:], wrapped)
	if err != nil {
		return "", fmt.Errorf("crypto: unwrap tenant key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, EncodingPrefix))
	if err != nil {
		return "", fmt.Errorf("crypto: decode ciphertext: %w", err)
	}
	plaintext, err := open(dek, raw)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// tenantDEK returns tenantID's unwrapped DEK, generating and persisting a
// fresh one on first use. It always generates a candidate key and wraps it
// before calling the store, even when a key likely already exists (the same
// "build it anyway, let ON CONFLICT decide" shape
// GetOrCreateClearingAccount's own doc comment describes): a wasted key
// generation is cheap, and it is what lets GetOrCreateWrappedDEK resolve a
// race between two concurrent first-use callers, even across processes, to
// exactly one winning DEK in a single round trip.
func (c *Cipher) tenantDEK(ctx context.Context, tenantID string) (dek []byte, shredded bool, err error) {
	candidate := make([]byte, dekSize)
	if _, err := rand.Read(candidate); err != nil {
		return nil, false, fmt.Errorf("crypto: generate candidate dek: %w", err)
	}
	wrappedCandidate, err := wrap(c.masterKey[:], candidate)
	if err != nil {
		return nil, false, fmt.Errorf("crypto: wrap candidate dek: %w", err)
	}
	wrapped, shredded, err := c.store.GetOrCreateWrappedDEK(ctx, tenantID, wrappedCandidate)
	if err != nil {
		return nil, false, fmt.Errorf("crypto: get or create tenant key: %w", err)
	}
	if shredded {
		return nil, true, nil
	}
	unwrapped, err := unwrap(c.masterKey[:], wrapped)
	if err != nil {
		return nil, false, fmt.Errorf("crypto: unwrap tenant key: %w", err)
	}
	return unwrapped, false, nil
}

// wrap is seal under the master key: how a tenant's DEK is protected at rest
// in crypto_keys.wrapped_dek.
func wrap(masterKey, dek []byte) ([]byte, error) { return seal(masterKey, dek) }

// unwrap is open under the master key, the inverse of wrap.
func unwrap(masterKey, wrapped []byte) ([]byte, error) { return open(masterKey, wrapped) }

// seal AES-256-GCM encrypts plaintext under key with a fresh random nonce,
// returning nonce || ciphertext (the nonce is not secret; it must simply
// never repeat for the same key, so it travels alongside the ciphertext it
// protects, the standard AEAD convention).
func seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// open is seal's inverse: it splits nonce || ciphertext and AES-256-GCM
// decrypts, authenticating the whole thing. Any tampering (or the wrong key)
// fails here with an opaque error: GCM deliberately does not distinguish
// "wrong key" from "corrupted ciphertext" from "truncated input" beyond what
// is checked below, so this package never tries to either.
func open(key, nonceAndCiphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(nonceAndCiphertext) < ns {
		return nil, errors.New("ciphertext shorter than nonce")
	}
	nonce, ciphertext := nonceAndCiphertext[:ns], nonceAndCiphertext[ns:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// newGCM builds the AES-256-GCM AEAD used by both seal and open.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm, nil
}
