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
	// ErrAPIKeyNotFound is returned when no unrevoked api_keys row matches a
	// presented key's hash. A transport layer should map it to 401 Unauthorized.
	ErrAPIKeyNotFound = errors.New("domain: api key not found")
	// ErrNonPositiveRate is returned when an FX conversion rate is zero or negative.
	ErrNonPositiveRate = errors.New("domain: fx rate must be positive")
	// ErrInvalidSpread is returned when an FX spread is negative or 100% or more.
	ErrInvalidSpread = errors.New("domain: fx spread out of range")
	// ErrConversionDust is returned when an FX conversion rounds a nonzero
	// source amount to zero in the quote currency.
	ErrConversionDust = errors.New("domain: conversion rounds to zero")
	// ErrFXRateNotFound is returned when no fx_rates row exists for a currency
	// pair, in either direction, at or before the requested time.
	ErrFXRateNotFound = errors.New("domain: fx rate not found")
	// ErrSameCurrencyRate is returned when an fx_rates row is inserted with
	// the same base and quote currency: a currency has no rate against itself.
	ErrSameCurrencyRate = errors.New("domain: fx rate base and quote must differ")
	// ErrNonPositiveConvertAmount is returned when a Convert request's source
	// amount is zero or negative. Zero would silently pass the conversion's
	// dust guard (a zero source converts to a zero result, and dust is only
	// detected when a nonzero source rounds to zero), and a negative amount
	// would run the conversion in reverse under a legitimate-looking request.
	ErrNonPositiveConvertAmount = errors.New("domain: convert source amount must be positive")
	// ErrSelfConversion is returned when a Convert request names the same
	// account as both the source and the destination.
	ErrSelfConversion = errors.New("domain: convert from and to account must differ")
	// ErrSameCurrencyConversion is returned when a Convert request's from and
	// to accounts share a currency: that is a transfer, not a conversion.
	ErrSameCurrencyConversion = errors.New("domain: convert from and to account must have different currencies")
	// ErrInvalidTenant is returned when a Tenant is missing a required field
	// (name) or carries a status outside TenantStatus.Valid().
	ErrInvalidTenant = errors.New("domain: invalid tenant")
	// ErrTenantNotFound is returned when no tenant matches the given id.
	ErrTenantNotFound = errors.New("domain: tenant not found")
	// ErrTenantNotActive is the sentinel matched via errors.Is for any
	// *TenantNotActiveError, regardless of which status (suspended or closed)
	// caused it. A transport layer maps it to 403 Forbidden (REST) or
	// codes.PermissionDenied (gRPC): the credential itself is valid, only the
	// tenant it belongs to is not.
	ErrTenantNotActive = errors.New("domain: tenant not active")
	// ErrTenantAlreadyExists is returned when CreateTenant is called with an
	// id that already has a tenant row.
	ErrTenantAlreadyExists = errors.New("domain: tenant already exists")
	// ErrAPIKeyExpired is the sentinel an auth resolver wraps alongside
	// ErrUnauthorized when a key's ExpiresAt has passed (Task 2.2): an expired
	// key is a dead credential, the same class as an unknown or revoked one,
	// and a transport layer maps it to 401 like any other ErrUnauthorized
	// without revealing that it was specifically expiry that failed.
	ErrAPIKeyExpired = errors.New("domain: api key expired")
	// ErrInsufficientScope is the sentinel matched via errors.Is for any
	// *InsufficientScopeError, regardless of which scope was required (Task
	// 2.2). A transport layer maps it to 403 Forbidden (REST) or
	// codes.PermissionDenied (gRPC): the credential itself is valid, it just
	// lacks the scope the operation needs.
	ErrInsufficientScope = errors.New("domain: insufficient scope")
	// ErrPolicyViolation is the sentinel matched via errors.Is for any
	// *PolicyViolationError, regardless of which TenantPolicy rule (max
	// transaction amount, daily volume, currency allowlist) tripped it (Task
	// 2.4b, audit A3.4). A transport layer maps it to 422 Unprocessable
	// Entity (REST) or codes.FailedPrecondition (gRPC): the request is
	// otherwise well-formed, it just violates a guardrail its tenant
	// configured.
	ErrPolicyViolation = errors.New("domain: tenant policy violation")
	// ErrInvalidTenantPolicy is returned when a TenantPolicy fails
	// TenantPolicy.Validate: a negative amount limit, or an
	// AllowedCurrencies entry that is not a well-formed three-letter
	// currency code.
	ErrInvalidTenantPolicy = errors.New("domain: invalid tenant policy")
)
