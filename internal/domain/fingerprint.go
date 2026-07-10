package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
	"time"
)

// Fingerprint returns a stable hex SHA-256 over the transaction's semantic
// content: each posting's account, signed amount, currency, and description,
// in order. Two requests that would post the same transaction share a
// fingerprint; any semantic change (a different amount, account, currency,
// description, or posting order) yields a different one. The transaction id is
// deliberately excluded so a client that retries without echoing an id still
// matches. It is used to detect a reused idempotency key carrying a different
// body.
//
// There is deliberately no transaction-level currency field to hash: ADR-014
// dropped the single transaction currency (a transaction can now span
// currencies), so currency moved into the per-posting fields below. This is a
// breaking change to the fingerprint scheme (ADR-014 decision 9): a key
// computed before this change will not match a key computed after it. That is
// accepted pre-real-money, with no durable in-flight keys to protect; see the
// ADR for the deferred versioning fix.
//
// Description stays in the per-posting hash on purpose: dropping it would
// collapse two transactions that differ only in a posting's description to
// the same fingerprint, so a reused idempotency key with a genuinely
// different body would silently replay the wrong stored transaction instead
// of returning 409.
//
// Each field is length-prefixed before being hashed, so field content can never
// be mistaken for a field boundary: two distinct transactions cannot collide
// even if a field carries bytes (for example an embedded NUL) that a plain
// separator scheme would let straddle a boundary.
func (t Transaction) Fingerprint() string {
	h := sha256.New()
	for _, p := range t.Postings {
		writeField(h, []byte(p.AccountID))
		writeField(h, []byte(strconv.FormatInt(p.Amount.Amount(), 10)))
		writeField(h, []byte(p.Amount.Currency()))
		writeField(h, []byte(p.Description))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ConvertRequestFingerprint returns a stable hex SHA-256 over a convert
// request's semantic content: the from account, the to account, and the
// source amount. Unlike Transaction.Fingerprint, which hashes the postings a
// transaction is made of, this hashes the REQUEST that produced them. That
// distinction is the point: a convert's idempotency key must be resolved
// before the FX rate is looked up (see internal/ledger's Convert), so a retry
// submitted after the rate moved still matches the same fingerprint and
// replays the original converted amount, instead of rebuilding a different
// set of postings, hashing those, and spuriously 409ing a legitimate retry.
//
// Each field is length-prefixed via writeField, the same self-delimiting
// framing Transaction.Fingerprint uses, so no field's bytes can be mistaken
// for a field boundary.
func ConvertRequestFingerprint(fromAccountID, toAccountID string, sourceAmount int64) string {
	h := sha256.New()
	writeField(h, []byte(fromAccountID))
	writeField(h, []byte(toAccountID))
	writeField(h, []byte(strconv.FormatInt(sourceAmount, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// writeField hashes b framed by its length, making the stream self-delimiting.
func writeField(h hash.Hash, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b))) //nolint:gosec // length is non-negative
	h.Write(n[:])
	h.Write(b)
}

// writeOptionalString hashes an optional string field as two length-prefixed
// frames: a one-byte presence marker (0 absent, 1 present), then the value
// (empty when absent). Always writing both frames, in the same order,
// regardless of presence keeps the stream shape identical across inputs, so
// the marker is the only thing that can vary field-count-wise; there is no
// way for an absent field and a present-but-different-length field to
// straddle a boundary and collide (see writeField's own doc comment for the
// general framing argument).
func writeOptionalString(h hash.Hash, s *string) {
	if s == nil {
		writeField(h, []byte{0})
		writeField(h, nil)
		return
	}
	writeField(h, []byte{1})
	writeField(h, []byte(*s))
}

// writeOptionalEffectiveAt hashes an optional effective_at as the same
// presence-marker framing writeOptionalString uses, with the value encoded
// as its UnixMicro decimal string when present. UnixMicro is used, not a
// formatted timestamp, for two reasons: it is timezone-invariant (an
// absolute instant, so a caller's local-time EffectiveAt and its UTC
// equivalent hash identically), and it matches the microsecond precision
// Postgres actually stores (migration 0018's effective_at column), so a
// value read back after a DB round trip hashes the same as the value that
// was written, even though a time.Time's in-memory nanosecond field does not
// survive that round trip exactly.
func writeOptionalEffectiveAt(h hash.Hash, ea *time.Time) {
	if ea == nil {
		writeField(h, []byte{0})
		writeField(h, nil)
		return
	}
	writeField(h, []byte{1})
	writeField(h, []byte(strconv.FormatInt(ea.UnixMicro(), 10)))
}

// fingerprintV2 returns the "v2" fingerprint scheme's hash: the same
// per-posting content Fingerprint (the "v1" scheme) hashes, plus Reference
// and EffectiveAt (Task 4.3, audit A1.3). Task 4.3 added both fields to
// Transaction, but the "v1" fingerprint never hashed either one, so a client
// could reuse an Idempotency-Key with identical postings and a DIFFERENT
// Reference (or EffectiveAt) and silently get back the original transaction,
// with the original reference, instead of a 409: a reconciliation hazard, since
// the caller has no way to tell its supplied reference was discarded. "v2"
// closes that gap by folding both fields into the hash, using the same
// writeField framing as every other field here, so a real ambiguity between a
// field's own bytes and a field boundary is never possible, matching the
// invariant Fingerprint's doc comment describes for the posting fields.
func (t Transaction) fingerprintV2() string {
	h := sha256.New()
	for _, p := range t.Postings {
		writeField(h, []byte(p.AccountID))
		writeField(h, []byte(strconv.FormatInt(p.Amount.Amount(), 10)))
		writeField(h, []byte(p.Amount.Currency()))
		writeField(h, []byte(p.Description))
	}
	writeOptionalString(h, t.Reference)
	writeOptionalEffectiveAt(h, t.EffectiveAt)
	return hex.EncodeToString(h.Sum(nil))
}

// CurrentFingerprintScheme names the fingerprint scheme this binary writes
// for every new idempotency key (Task 2.3, audit A1.6). It is stored
// alongside the fingerprint itself (idempotency_keys.fingerprint_scheme,
// migration 0013), so a stored key always carries the scheme that produced
// it.
//
// Bump this constant, and add a case to TransactionFingerprint and
// ConvertFingerprint for the new scheme, whenever the fingerprint's content
// or framing changes. Keep the old scheme's case computing the old
// (byte-identical) output: a key stored under "v1" must keep comparing
// against a "v1" recomputation forever, even after "v2" becomes current.
// This is what makes a fingerprint change non-breaking: old stored keys
// recompute under the scheme that produced them instead of false-conflicting
// against a new scheme's output.
//
// Bumped to "v2" for Task 4.3 (audit A1.3): see fingerprintV2's doc comment
// for why "v1" needed replacing rather than just growing new fields under the
// same name. Every idempotency key this binary writes from here on carries
// "v2"; a key already stored under "v1" keeps comparing against
// Transaction.Fingerprint() (the "v1" case below), never against
// fingerprintV2, so nothing written before this change is invalidated.
const CurrentFingerprintScheme = "v2"

// TransactionFingerprint computes t's fingerprint under the named scheme. ok
// is false if scheme is not one this binary knows how to compute (for
// example a key written by a newer binary, then read by this one after a
// downgrade); the caller must treat that as fail-closed, never as a match.
func TransactionFingerprint(scheme string, t Transaction) (fp string, ok bool) {
	switch scheme {
	case "v1":
		return t.Fingerprint(), true
	case "v2":
		return t.fingerprintV2(), true
	default:
		return "", false
	}
}

// ConvertFingerprint computes a convert request's fingerprint under the
// named scheme. ok is false for a scheme this binary does not know how to
// compute; see TransactionFingerprint.
//
// A convert request has no reference or effective_at field to hash (see
// ConvertRequestFingerprint's own doc comment: it hashes the REQUEST, not the
// postings Convert builds from it), so "v2" is registered here as identical to
// "v1": there is nothing for the Task 4.3 fingerprint change to add on the
// convert path. This keeps CurrentFingerprintScheme a single shared constant
// across both insert paths (see Post and Convert) without forcing a
// meaningless divergence in the convert request's hash content.
func ConvertFingerprint(scheme, fromAccountID, toAccountID string, sourceAmount int64) (fp string, ok bool) {
	switch scheme {
	case "v1", "v2":
		return ConvertRequestFingerprint(fromAccountID, toAccountID, sourceAmount), true
	default:
		return "", false
	}
}
