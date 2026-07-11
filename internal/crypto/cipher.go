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
// complement) is destroying its CURRENT DEK version: crypto_keys.wrapped_dek
// for that version is set NULL and shredded_at is stamped
// (domain.Repository.ShredTenantCryptoKey). This package's Decrypt treats a
// shredded version's ciphertext as RedactedMarker, not an error, so reads
// keep working; it never needs the plaintext or the destroyed key again.
// Shredding never touches postings.description or audit_log.after: those
// bytes are unchanged, so the audit chain's row_hash (computed over those
// exact stored bytes, domain.ComputeAuditRowHash) still recomputes
// identically and the chain still verifies after a shred. This is the
// money-critical property the whole package exists to preserve; see
// AuditService.Verify's own doc comment for why it never decrypts.
//
// DEKs are VERSIONED (ADR-018): a tenant can hold a sequence of versions
// over time, keyed on (tenant_id, version) in crypto_keys (migration 0028).
// A shred destroys only the CURRENT (highest) version; the tenant's very
// next Encrypt call mints a fresh, forward version and keeps working, so an
// erasure request erases PAST PII without bricking the tenant's ability to
// post, convert, or reverse (those write system-generated narration, not
// personal data, and must keep succeeding after a shred). See Encrypt and
// ErrTenantKeyShredded's own doc comments for the exact mechanics.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
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
// The "v1" tag names the SCHEME (nonce||ciphertext under AES-256-GCM), not
// the DEK version: a stored value looks like "enc:v1:<version>:<base64>",
// where <version> is the tenant's DEK version this exact ciphertext was
// sealed under (ADR-018), so a later Decrypt always knows which version's
// key to unwrap, even after the tenant has moved on to a newer version. The
// "v1" scheme tag leaves room for a future encoding change the same way
// domain.CurrentFingerprintScheme does for the idempotency fingerprint: a
// reader that knows only "v1" can still fail closed on an unrecognized future
// prefix instead of misinterpreting its bytes. See parseVersionedCiphertext
// for exactly how the version segment is parsed, including its handling of
// a body with no version segment at all.
const EncodingPrefix = "enc:v1:"

// RedactedMarker is what Decrypt returns, with no error, for a ciphertext
// description whose DEK version has been shredded (Task 6.2, audit A9.3;
// versioned per ADR-018): the whole point of crypto-shredding is that reads
// keep working after an erasure request, they just can no longer recover the
// original content.
const RedactedMarker = "[redacted: erased]"

// ErrTenantKeyShredded is a defensive sentinel: since ADR-018 versioned
// per-tenant DEKs, Encrypt no longer fails closed just because a tenant's
// CURRENT key version has been shredded, it mints a fresh, forward version
// and keeps going (see Encrypt's own doc comment), so this should no longer
// be reachable on any normal post/convert/reverse path. It remains exported,
// and is still returned in the one adversarial case currentOrMintedDEK
// cannot resolve (every version it tries to mint, up to mintRetryLimit
// attempts, loses a race to a concurrent shred targeting that exact
// version): callers (internal/api's toHumaErr, internal/grpcserver's
// toStatus) still map it to a clear client error rather than a generic 500,
// in case this ever surfaces.
var ErrTenantKeyShredded = errors.New("crypto: tenant key has been shredded")

// KeyStore is the persistence port for per-tenant, VERSIONED Data
// Encryption Keys (Task 6.2, audit A9.3; versioned per ADR-018): the
// crypto_keys table (migrations 0027, 0028) behind it, keyed on
// (tenant_id, version). internal/postgres.Repository implements this
// directly, alongside domain.Repository, so cmd/server can hand the same
// *postgres.Repository value to both NewCipher and postgres.NewRepository.
type KeyStore interface {
	// CurrentTenantDEK returns tenantID's CURRENT (highest-version) key row,
	// whatever its shredded state, so Encrypt can decide whether to reuse it
	// or mint a fresh, forward version (see currentOrMintedDEK). found is
	// false if the tenant has never encrypted anything and was never
	// shredded either: version is meaningless in that case (Encrypt treats
	// it as "the next version to mint is 1").
	CurrentTenantDEK(ctx context.Context, tenantID string) (wrappedDEK []byte, version int, shredded, found bool, err error)

	// MintTenantDEKVersion atomically creates tenantID's crypto_keys row at
	// exactly version, wrapping candidateWrappedDEK (an INSERT ... ON
	// CONFLICT DO UPDATE that forces RETURNING to yield whichever row wins a
	// race between concurrent callers targeting the SAME version, the same
	// "GetOrCreateClearingAccount" pattern
	// internal/postgres/queries/accounts.sql already established).
	// candidateWrappedDEK is generated and wrapped by the CALLER (Cipher),
	// not the store, since only Cipher holds the master key: the store's job
	// is purely to persist bytes, never to know what they mean.
	//
	// shredded is true if the row this call ends up returning (whoever's
	// candidate won the race) is already shredded: an extremely unlikely
	// race against a shred call that targeted this exact version concurrently.
	// The caller must handle that by minting the NEXT version rather than
	// assuming a mint always yields a usable key.
	MintTenantDEKVersion(ctx context.Context, tenantID string, version int, candidateWrappedDEK []byte) (wrappedDEK []byte, shredded bool, err error)

	// TenantDEKVersion returns tenantID's key at exactly the given version,
	// for a decrypt whose stored ciphertext names that version, without ever
	// creating one: a description already carrying EncodingPrefix implies a
	// DEK existed for that exact version at encrypt time, so found being
	// false is a genuine inconsistency, not the normal first-use case
	// CurrentTenantDEK/MintTenantDEKVersion handle.
	TenantDEKVersion(ctx context.Context, tenantID string, version int) (wrappedDEK []byte, shredded, found bool, err error)
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

// mintRetryLimit bounds currentOrMintedDEK's retry loop when a freshly
// minted version keeps losing a race to a concurrent shred targeting the
// exact same version number: an adversarial scenario this defensively caps
// rather than loops on forever. See ErrTenantKeyShredded's own doc comment.
const mintRetryLimit = 5

// Encrypt returns plaintext encrypted under tenantID's CURRENT data key
// version, as a self-describing string a later Decrypt call can recognize
// (EncodingPrefix + the DEK version + base64(nonce || ciphertext); see
// EncodingPrefix's own doc comment for the exact shape). An empty plaintext
// is returned unchanged ("" in, "" out): there is nothing to protect in an
// absent description, and encrypting it would turn "no description" into
// indistinguishable-from-real ciphertext on read back, breaking the "empty
// stays empty" contract internal/ledger's write path documents.
//
// Per ADR-018, Encrypt never fails closed just because tenantID's current
// key version has been shredded: it mints a fresh, forward version and uses
// that instead (see currentOrMintedDEK), so a data-subject erasure request
// erases that tenant's PAST descriptions without bricking its ability to
// post, convert, or reverse afterward. See ErrTenantKeyShredded's own doc
// comment for the one adversarial case this can still surface in.
func (c *Cipher) Encrypt(ctx context.Context, tenantID, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	dek, version, err := c.currentOrMintedDEK(ctx, tenantID)
	if err != nil {
		return "", err
	}
	sealed, err := seal(dek, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("crypto: encrypt: %w", err)
	}
	return fmt.Sprintf("%s%d:%s", EncodingPrefix, version, base64.StdEncoding.EncodeToString(sealed)), nil
}

// Decrypt returns stored's plaintext content, or a well-defined non-error
// substitute in two cases that are not failures:
//
//   - stored does not carry EncodingPrefix: it is legacy, pre-Task-6.2
//     plaintext (or already empty), returned unchanged. This is the backward
//     compatibility guarantee: every posting description written before this
//     feature existed reads back exactly as it always did.
//   - the DEK version stored's ciphertext names has been shredded:
//     RedactedMarker is returned, not an error, so a read of a
//     crypto-shredded tenant's history keeps working (Task 6.2, audit
//     A9.3's core requirement) instead of failing every
//     GetTransaction/ListTransactions/Statement/audit-read call for that
//     tenant forever. A LATER version, if the tenant has since posted again
//     (ADR-018), decrypts normally: shredding is scoped to the version it
//     targeted, never to every version a tenant has ever held.
//
// Any other failure (no key material at all for the named version, a
// corrupt or tampered ciphertext, an unwrap failure) is a genuine error:
// Decrypt only ever silently substitutes for the two well-understood,
// expected cases above.
func (c *Cipher) Decrypt(ctx context.Context, tenantID, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, EncodingPrefix) {
		return stored, nil
	}
	version, encoded := parseVersionedCiphertext(stored)
	wrapped, shredded, found, err := c.store.TenantDEKVersion(ctx, tenantID, version)
	if err != nil {
		return "", fmt.Errorf("crypto: look up tenant key version %d: %w", version, err)
	}
	if !found {
		return "", fmt.Errorf("crypto: tenant %s has no key material for version %d, but stored value is ciphertext", tenantID, version)
	}
	if shredded {
		return RedactedMarker, nil
	}
	dek, err := unwrap(c.masterKey[:], wrapped)
	if err != nil {
		return "", fmt.Errorf("crypto: unwrap tenant key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("crypto: decode ciphertext: %w", err)
	}
	plaintext, err := open(dek, raw)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// parseVersionedCiphertext splits stored's body (everything after
// EncodingPrefix) into the DEK version it names and the remaining
// base64(nonce||ciphertext). A body with no ":" separator, or one whose
// segment before the first ":" is not a positive integer, is treated as a
// pre-versioning "enc:v1:<base64>" value and given an implicit version of 1:
// this feature had not shipped outside this branch's own tests before
// ADR-018 added versioning, so no such value is known to actually exist, but
// Decrypt handles it defensively rather than erroring on it.
func parseVersionedCiphertext(stored string) (version int, encoded string) {
	body := strings.TrimPrefix(stored, EncodingPrefix)
	if idx := strings.IndexByte(body, ':'); idx >= 0 {
		if v, err := strconv.Atoi(body[:idx]); err == nil && v > 0 {
			return v, body[idx+1:]
		}
	}
	return 1, body
}

// currentOrMintedDEK returns tenantID's unwrapped CURRENT DEK and the
// version it belongs to, minting a fresh, forward version whenever there is
// no current one yet (first use) or the current one has been shredded
// (ADR-018): a shred destroys a tenant's ability to encrypt under the
// version it targeted, never its ability to encrypt at all. It always
// generates a candidate key and wraps it before calling the store when
// minting, even though a concurrent caller might already be minting the
// same version (the same "build it anyway, let ON CONFLICT decide" shape
// GetOrCreateClearingAccount's own doc comment describes): a wasted key
// generation is cheap, and it is what lets MintTenantDEKVersion resolve a
// race between two concurrent callers targeting the same version, even
// across processes, to exactly one winning DEK in a single round trip.
//
// The mint loop only ever advances past mintRetryLimit attempts in the
// adversarial case of a shred call racing this exact version repeatedly;
// see ErrTenantKeyShredded's own doc comment.
func (c *Cipher) currentOrMintedDEK(ctx context.Context, tenantID string) (dek []byte, version int, err error) {
	wrapped, currentVersion, shredded, found, err := c.store.CurrentTenantDEK(ctx, tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("crypto: get current tenant key: %w", err)
	}
	if found && !shredded {
		unwrapped, err := unwrap(c.masterKey[:], wrapped)
		if err != nil {
			return nil, 0, fmt.Errorf("crypto: unwrap tenant key: %w", err)
		}
		return unwrapped, currentVersion, nil
	}

	nextVersion := currentVersion + 1 // currentVersion is 0 when !found, so this is 1.
	for attempt := 0; attempt < mintRetryLimit; attempt++ {
		candidate := make([]byte, dekSize)
		if _, err := rand.Read(candidate); err != nil {
			return nil, 0, fmt.Errorf("crypto: generate candidate dek: %w", err)
		}
		wrappedCandidate, err := wrap(c.masterKey[:], candidate)
		if err != nil {
			return nil, 0, fmt.Errorf("crypto: wrap candidate dek: %w", err)
		}
		minted, mintedShredded, err := c.store.MintTenantDEKVersion(ctx, tenantID, nextVersion, wrappedCandidate)
		if err != nil {
			return nil, 0, fmt.Errorf("crypto: mint tenant key version %d: %w", nextVersion, err)
		}
		if !mintedShredded {
			unwrapped, err := unwrap(c.masterKey[:], minted)
			if err != nil {
				return nil, 0, fmt.Errorf("crypto: unwrap freshly minted tenant key: %w", err)
			}
			return unwrapped, nextVersion, nil
		}
		nextVersion++
	}
	return nil, 0, ErrTenantKeyShredded
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
