package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
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
const CurrentFingerprintScheme = "v1"

// TransactionFingerprint computes t's fingerprint under the named scheme. ok
// is false if scheme is not one this binary knows how to compute (for
// example a key written by a newer binary, then read by this one after a
// downgrade); the caller must treat that as fail-closed, never as a match.
func TransactionFingerprint(scheme string, t Transaction) (fp string, ok bool) {
	switch scheme {
	case "v1":
		return t.Fingerprint(), true
	default:
		return "", false
	}
}

// ConvertFingerprint computes a convert request's fingerprint under the
// named scheme. ok is false for a scheme this binary does not know how to
// compute; see TransactionFingerprint.
func ConvertFingerprint(scheme, fromAccountID, toAccountID string, sourceAmount int64) (fp string, ok bool) {
	switch scheme {
	case "v1":
		return ConvertRequestFingerprint(fromAccountID, toAccountID, sourceAmount), true
	default:
		return "", false
	}
}
