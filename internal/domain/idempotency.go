package domain

// Idempotency carries the client's Idempotency-Key into a write so a retry can
// be recognized and replayed. A nil *Idempotency means the caller supplied no
// key, and the write proceeds without idempotency.
type Idempotency struct {
	Key string
}

// IdempotencyRecord is a stored idempotency key: the fingerprint of the original
// request and the transaction that request produced.
type IdempotencyRecord struct {
	Key           string
	Fingerprint   string
	TransactionID string
}
