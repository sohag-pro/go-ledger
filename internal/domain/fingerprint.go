package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
)

// Fingerprint returns a stable hex SHA-256 over the transaction's semantic
// content: the shared currency and each posting's account, signed amount, and
// description, in order. Two requests that would post the same transaction share
// a fingerprint; any semantic change (a different amount, account, currency,
// description, or posting order) yields a different one. The transaction id is
// deliberately excluded so a client that retries without echoing an id still
// matches. It is used to detect a reused idempotency key carrying a different
// body.
//
// Each field is length-prefixed before being hashed, so field content can never
// be mistaken for a field boundary: two distinct transactions cannot collide
// even if a field carries bytes (for example an embedded NUL) that a plain
// separator scheme would let straddle a boundary.
func (t Transaction) Fingerprint() string {
	h := sha256.New()
	if len(t.Postings) > 0 {
		writeField(h, []byte(t.Postings[0].Amount.Currency()))
	}
	for _, p := range t.Postings {
		writeField(h, []byte(p.AccountID))
		writeField(h, []byte(strconv.FormatInt(p.Amount.Amount(), 10)))
		writeField(h, []byte(p.Description))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeField hashes b framed by its length, making the stream self-delimiting.
func writeField(h hash.Hash, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b))) //nolint:gosec // length is non-negative
	h.Write(n[:])
	h.Write(b)
}
