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
	// ErrInvalidAccount is returned when an Account is missing required
	// fields, or carries a Status outside AccountStatus.Valid() (Task 5.5,
	// audit A1.5), the same dual role ErrInvalidTenant plays for Tenant.
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
	// ErrCannotReverseReversal is returned when ReverseTransaction is asked to
	// reverse a transaction that is itself a reversal (its
	// ReversesTransactionID is already set). Reversing a reversal has no
	// meaning in this model: postings are append-only (ADR-001), so undoing a
	// correction is itself just a new, forward correction, and only ever of
	// an ORIGINAL transaction (Task 4.2, audit A1.2).
	ErrCannotReverseReversal = errors.New("domain: cannot reverse a reversal")
	// ErrTransactionAlreadyReversed signals that a transaction already has a
	// reversal linked to it (the transactions_one_reversal_idx unique index,
	// migration 0017). Like ErrDuplicateIdempotencyKey, it is an internal
	// control-flow signal: TransactionService.ReverseTransaction catches it
	// and reads back the existing reversal instead of surfacing it to a
	// caller, so a concurrent double-reverse resolves to exactly one
	// reversal, whichever attempt's insert won the race.
	ErrTransactionAlreadyReversed = errors.New("domain: transaction already reversed")
	// ErrInvalidReference is returned when a Transaction's Reference is
	// present but empty (Task 4.3, audit A1.3): a client that sets the field
	// at all must give it real content, a bare "" is not a meaningful
	// external reference.
	ErrInvalidReference = errors.New("domain: reference must not be empty when present")
	// ErrReferenceTooLong is returned when a Transaction's Reference exceeds
	// MaxTransactionReferenceLen.
	ErrReferenceTooLong = errors.New("domain: reference too long")
	// ErrDuplicateReference is returned when a transaction is posted with a
	// Reference that already exists for the same tenant
	// (transactions_tenant_reference_idx, migration 0018). A transport layer
	// should map it to 409 Conflict (REST) or codes.AlreadyExists (gRPC),
	// distinct from ErrIdempotencyConflict: this is a different request that
	// happens to reuse someone else's external reference, not a retry of the
	// same one.
	ErrDuplicateReference = errors.New("domain: reference already exists for this tenant")
	// ErrInvalidWebhookURL is returned when a WebhookSubscription's URL is
	// empty, unparsable, not http/https, or has no host (Task 4.1, audit
	// A7.1). Checked before a secret is generated or any row is written.
	ErrInvalidWebhookURL = errors.New("domain: webhook url must be an absolute http or https URL")
	// ErrWebhookSubscriptionNotFound is returned when no webhook_subscriptions
	// row matches the given id.
	ErrWebhookSubscriptionNotFound = errors.New("domain: webhook subscription not found")
	// ErrAccountNotActive is the sentinel matched via errors.Is for any
	// *AccountNotActiveError, regardless of which status (frozen or closed)
	// caused it (Task 5.5, audit A1.5). A transport layer maps it to 422
	// Unprocessable Entity (REST) or codes.FailedPrecondition (gRPC): the
	// request is otherwise well-formed, it just touches an account that is
	// not currently postable.
	ErrAccountNotActive = errors.New("domain: account not active")
	// ErrMinBalanceBreach is the sentinel matched via errors.Is for any
	// *MinBalanceBreachError (Task 5.5, audit A1.5). A transport layer maps
	// it to 422 Unprocessable Entity (REST) or codes.FailedPrecondition
	// (gRPC), the same class as ErrAccountNotActive: the request is
	// otherwise well-formed, it just would take an account below its
	// configured floor.
	ErrMinBalanceBreach = errors.New("domain: posting would breach account minimum balance")
	// ErrPartyReferenceTooLong is returned when an Account's PartyReference
	// exceeds MaxPartyReferenceLen (Task 6.1, audit A9.1).
	ErrPartyReferenceTooLong = errors.New("domain: account party reference too long")
	// ErrPartyTypeTooLong is returned when an Account's PartyType exceeds
	// MaxPartyTypeLen (Task 6.1, audit A9.1).
	ErrPartyTypeTooLong = errors.New("domain: account party type too long")
	// ErrInvalidDispute is returned when a Dispute is missing a required
	// field (TransactionID or Reason) or carries a Status outside
	// DisputeStatus.Valid() (Task 6.3, audit A9.2).
	ErrInvalidDispute = errors.New("domain: invalid dispute")
	// ErrDisputeReasonTooLong is returned when a Dispute's Reason exceeds
	// MaxDisputeReasonLen (Task 6.3, audit A9.2).
	ErrDisputeReasonTooLong = errors.New("domain: dispute reason too long")
	// ErrDisputeNotFound is returned when no dispute matches the given id
	// within the tenant (Task 6.3, audit A9.2).
	ErrDisputeNotFound = errors.New("domain: dispute not found")
	// ErrDisputeAlreadyResolved is returned when Resolve is called for a
	// dispute whose Status is no longer "open" (Task 6.3, audit A9.2): a
	// dispute is resolved at most once, and resolving one twice (whether
	// sequentially or racing concurrently, see postgres.Repository.ResolveDispute's
	// guarded UPDATE) is rejected with this error rather than silently
	// replaying or overwriting the first resolution.
	ErrDisputeAlreadyResolved = errors.New("domain: dispute already resolved")
	// ErrInvalidDisputeAction is returned when Resolve is called with an
	// action that is neither "reverse" nor "reject" (Task 6.3, audit A9.2).
	// The REST layer's enum schema tag already rejects any other value
	// before it reaches the service, so this is a defensive backstop for any
	// other caller (for example a future gRPC surface) that does not share
	// that validation.
	ErrInvalidDisputeAction = errors.New("domain: dispute action must be reverse or reject")
	// ErrInvalidHierarchy is a rejected parent change: a self-parent, a cycle, or a
	// child whose currency differs from its parent's. Mapped to 422.
	ErrInvalidHierarchy = errors.New("domain: invalid account hierarchy")
	// ErrParentNotFound is a parent_id that names no account in the tenant. Mapped to 422.
	ErrParentNotFound = errors.New("domain: parent account not found")
	// ErrPendingTransactionNotFound is returned when no pending_transactions
	// row matches the given id within the tenant (ADR-025, Week 13).
	ErrPendingTransactionNotFound = errors.New("domain: pending transaction not found")
	// ErrInvalidPendingTransaction is returned when a PendingTransaction
	// carries an unrecognized Kind, an empty Payload, or an empty
	// ThresholdCcy/CreatedBy (ADR-025, Week 13).
	ErrInvalidPendingTransaction = errors.New("domain: invalid pending transaction")
)
