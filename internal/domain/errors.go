// Package domain holds the double-entry accounting model: Money, Account,
// Transaction, and Posting. It has no dependencies on storage or transport.
// The balance invariant (every transaction's postings sum to zero) is enforced
// here by Transaction.Validate and is the core guarantee of the whole service.
package domain

import "errors"

var (
	// ErrCurrencyMismatch is returned when an operation combines Money values
	// of different currencies.
	ErrCurrencyMismatch = errors.New("domain: currency mismatch")
	// ErrOverflow is returned when an arithmetic result exceeds int64 range.
	ErrOverflow = errors.New("domain: amount overflow")
	// ErrInvalidCurrency is returned when a Currency is not a valid code.
	ErrInvalidCurrency = errors.New("domain: invalid currency")
	// ErrUnbalanced is returned when a transaction's postings do not sum to zero.
	ErrUnbalanced = errors.New("domain: transaction does not balance")
	// ErrTooFewPostings is returned when a transaction has fewer than two postings.
	ErrTooFewPostings = errors.New("domain: transaction needs at least two postings")
	// ErrInvalidAccountType is returned when an AccountType is out of range.
	ErrInvalidAccountType = errors.New("domain: invalid account type")
	// ErrInvalidAccount is returned when an Account is missing required fields.
	ErrInvalidAccount = errors.New("domain: invalid account")
	// ErrInvalidPosting is returned when a posting is missing an account id.
	ErrInvalidPosting = errors.New("domain: invalid posting")
	// ErrDescriptionTooLong is returned when a posting description exceeds
	// MaxPostingDescriptionLen.
	ErrDescriptionTooLong = errors.New("domain: posting description too long")
	// ErrAccountNotFound is returned when no account matches the given id.
	ErrAccountNotFound = errors.New("domain: account not found")
	// ErrTransactionNotFound is returned when no transaction matches the given id.
	ErrTransactionNotFound = errors.New("domain: transaction not found")
	// ErrConflict is returned when a write could not be serialized after the
	// adapter exhausted its retries. It is transient: the caller may retry, and a
	// transport layer should map it to 503 (or 409), not 500.
	ErrConflict = errors.New("domain: write conflict, retries exhausted")
	// ErrDuplicateTransaction is returned when a transaction is created with an id
	// that already exists. A transport layer should map it to 409 Conflict.
	ErrDuplicateTransaction = errors.New("domain: transaction already exists")
	// ErrIdempotencyConflict is returned when an Idempotency-Key is reused with a
	// different request body. A transport layer should map it to 409 Conflict.
	ErrIdempotencyConflict = errors.New("domain: idempotency key reused with a different request")
	// ErrDuplicateIdempotencyKey signals that an idempotency key already exists.
	// It is an internal control-flow signal: the service turns it into a replay,
	// so it is never surfaced to transport directly.
	ErrDuplicateIdempotencyKey = errors.New("domain: idempotency key already exists")
	// ErrIdempotencyKeyNotFound is returned when a lookup finds no row for a key.
	ErrIdempotencyKeyNotFound = errors.New("domain: idempotency key not found")
)
