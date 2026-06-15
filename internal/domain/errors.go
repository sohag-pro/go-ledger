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
)
