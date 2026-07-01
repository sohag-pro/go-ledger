package domain

import (
	"crypto/sha256"
	"encoding/hex"
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
func (t Transaction) Fingerprint() string {
	h := sha256.New()
	if len(t.Postings) > 0 {
		h.Write([]byte(t.Postings[0].Amount.Currency()))
	}
	for _, p := range t.Postings {
		// A NUL separator between fields so distinct field boundaries cannot
		// collide (for example account "ab" + "" vs "a" + "b").
		h.Write([]byte{0})
		h.Write([]byte(p.AccountID))
		h.Write([]byte{0})
		h.Write([]byte(strconv.FormatInt(p.Amount.Amount(), 10)))
		h.Write([]byte{0})
		h.Write([]byte(p.Description))
	}
	return hex.EncodeToString(h.Sum(nil))
}
