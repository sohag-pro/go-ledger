// Package grpcserver is the gRPC adapter for the ledger. It implements the
// generated LedgerServiceServer by translating protobuf to domain types and
// calling the same ledger services the REST API uses. It holds no business
// rules.
package grpcserver

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/crypto"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// toStatus maps a domain error to a gRPC status error. It mirrors the REST
// layer's toHumaErr: missing resources are NotFound, a duplicate or reused
// idempotency key is AlreadyExists, validation and invariant failures are
// InvalidArgument, an exhausted serialization conflict is Unavailable, and
// anything unrecognized is Internal without leaking internals. A duplicate
// external reference (Task 4.3, audit A1.3) is also AlreadyExists, but its
// own distinct case: a different request reusing someone else's reference,
// not a retried request under the same idempotency key.
func toStatus(err error) error {
	// A *domain.PolicyViolationError (Task 2.4b, audit A3.4) is checked via
	// errors.As, before the switch below, so its Error() message (naming the
	// exact rule, currency, and amount/limit) reaches the client instead of
	// a generic message. It maps to FailedPrecondition, not InvalidArgument:
	// the request itself is well-formed, it just fails a guardrail its
	// tenant configured, which is exactly what FailedPrecondition means.
	var policyErr *domain.PolicyViolationError
	if errors.As(err, &policyErr) {
		return status.Error(codes.FailedPrecondition, policyErr.Error())
	}
	// *domain.AccountNotActiveError and *domain.MinBalanceBreachError (Task
	// 5.5, audit A1.5) are checked the same way, before the switch below:
	// both map to FailedPrecondition, the same class as a tripped
	// TenantPolicy guardrail, and each carries a message naming the exact
	// account and status/floor involved.
	var accountNotActiveErr *domain.AccountNotActiveError
	if errors.As(err, &accountNotActiveErr) {
		return status.Error(codes.FailedPrecondition, accountNotActiveErr.Error())
	}
	var minBalanceErr *domain.MinBalanceBreachError
	if errors.As(err, &minBalanceErr) {
		return status.Error(codes.FailedPrecondition, minBalanceErr.Error())
	}
	// *ledger.ScreeningRejectedError (Task 6.1, audit A9.1): an external
	// screening/compliance hook explicitly vetoed the post. FailedPrecondition,
	// the same class as the checks above: the request itself is well-formed,
	// it just fails a check, here naming the hook's own reason.
	var screeningRejectedErr *ledger.ScreeningRejectedError
	if errors.As(err, &screeningRejectedErr) {
		return status.Error(codes.FailedPrecondition, screeningRejectedErr.Error())
	}
	// *ledger.HeldForApprovalError (audit A: gRPC held mapping): the write
	// exceeded the approval threshold and was stored as a pending instead of
	// posted (ADR-025). REST reports this as 202 with the pending resource;
	// gRPC has no such body, so without this case it would fall through to a
	// generic Internal and look like a server fault. Map it to
	// FailedPrecondition, the same class as the guardrail checks above (the
	// request is well-formed, the current state just requires approval first),
	// and name the pending id so a caller can approve it via the REST
	// /v1/pending surface. The pending IS committed by the time this returns.
	if pending, held := ledger.AsHeldForApproval(err); held {
		return status.Errorf(codes.FailedPrecondition,
			"transaction held for approval: pending %s (approve via the /v1/pending REST API)", pending.ID)
	}

	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAccountNotFound):
		return status.Error(codes.NotFound, "account not found")
	case errors.Is(err, domain.ErrTransactionNotFound):
		return status.Error(codes.NotFound, "transaction not found")
	case errors.Is(err, domain.ErrCannotReverseReversal):
		return status.Error(codes.FailedPrecondition, "cannot reverse a transaction that is itself a reversal")
	case errors.Is(err, domain.ErrIdempotencyKeyNotFound):
		return status.Error(codes.NotFound, "idempotency key not found")
	case errors.Is(err, domain.ErrDuplicateTransaction):
		return status.Error(codes.AlreadyExists, "transaction already exists")
	case errors.Is(err, domain.ErrDuplicateReference):
		return status.Error(codes.AlreadyExists, "reference already exists for this tenant")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists, "idempotency key was reused with a different request body")
	case errors.Is(err, domain.ErrConflict):
		return status.Error(codes.Unavailable, "write conflict, please retry")
	// ledger.ErrScreeningUnavailable (Task 6.1, audit A9.1): an AMBIGUOUS
	// screening failure (not an explicit veto), fail closed but presented as
	// retryable, the same class as ErrConflict above.
	case errors.Is(err, ledger.ErrScreeningUnavailable):
		return status.Error(codes.Unavailable, "screening system unavailable, please retry")
	case errors.Is(err, domain.ErrUnbalanced):
		return status.Error(codes.InvalidArgument, "transaction postings must sum to zero")
	case errors.Is(err, domain.ErrCurrencyMismatch):
		return status.Error(codes.InvalidArgument, "every posting must be in the transaction's currency, and that currency must match the account's currency")
	case errors.Is(err, domain.ErrTooFewPostings):
		return status.Error(codes.InvalidArgument, "a transaction needs at least two postings")
	case errors.Is(err, domain.ErrInvalidPosting):
		return status.Error(codes.InvalidArgument, "a posting is missing its account id")
	case errors.Is(err, domain.ErrInvalidAccount):
		return status.Error(codes.InvalidArgument, "account is missing a required field")
	case errors.Is(err, domain.ErrInvalidAccountType):
		return status.Error(codes.InvalidArgument, "invalid account type")
	case errors.Is(err, domain.ErrInvalidCurrency):
		return status.Error(codes.InvalidArgument, "invalid currency code (expected three uppercase letters)")
	case errors.Is(err, domain.ErrDescriptionTooLong):
		return status.Error(codes.InvalidArgument, "posting description is too long")
	case errors.Is(err, domain.ErrInvalidReference):
		return status.Error(codes.InvalidArgument, "reference must not be empty when present")
	case errors.Is(err, domain.ErrReferenceTooLong):
		return status.Error(codes.InvalidArgument, "reference is too long")
	// domain.ErrPartyReferenceTooLong and domain.ErrPartyTypeTooLong (Task
	// 6.1, audit A9.1): unlike REST, where the maxLength JSON schema tag on
	// PartyReference/PartyType (internal/api/accounts.go) rejects an
	// over-length value before it reaches the domain, the gRPC proto has no
	// equivalent length constraint on these fields, so this is the only thing
	// that catches an over-length party_reference/party_type over gRPC.
	// InvalidArgument, the same class as ErrDescriptionTooLong and
	// ErrReferenceTooLong above.
	case errors.Is(err, domain.ErrPartyReferenceTooLong):
		return status.Error(codes.InvalidArgument, "account party reference is too long")
	case errors.Is(err, domain.ErrPartyTypeTooLong):
		return status.Error(codes.InvalidArgument, "account party type is too long")
	case errors.Is(err, domain.ErrOverflow):
		return status.Error(codes.InvalidArgument, "amount is out of range")
	case errors.Is(err, domain.ErrConversionDust):
		return status.Error(codes.InvalidArgument, "conversion amount rounds to zero in the destination currency")
	case errors.Is(err, domain.ErrNonPositiveRate):
		return status.Error(codes.InvalidArgument, "fx rate must be positive")
	case errors.Is(err, domain.ErrInvalidSpread):
		return status.Error(codes.InvalidArgument, "fx spread is out of range")
	case errors.Is(err, domain.ErrFXRateNotFound):
		return status.Error(codes.InvalidArgument, "no fx rate is configured for this currency pair")
	case errors.Is(err, domain.ErrNonPositiveConvertAmount):
		return status.Error(codes.InvalidArgument, "source_amount must be positive")
	case errors.Is(err, domain.ErrSelfConversion):
		return status.Error(codes.InvalidArgument, "from_account and to_account must differ")
	case errors.Is(err, domain.ErrSameCurrencyConversion):
		return status.Error(codes.InvalidArgument, "from_account and to_account must have different currencies")
	// crypto.ErrTenantKeyShredded (Task 6.2, audit A9.3 review; ADR-018): see
	// toHumaErr's identical case (internal/api/errors.go) for the full
	// reasoning. FailedPrecondition, the same class as
	// PolicyViolationError/AccountNotActiveError/ScreeningRejectedError
	// above, not AlreadyExists: this is not a duplicate or an
	// already-existing resource, just a well-formed request that (in the one
	// adversarial case this can still occur) fails an operational
	// precondition on its tenant's encryption key.
	case errors.Is(err, crypto.ErrTenantKeyShredded):
		return status.Error(codes.FailedPrecondition, "tenant PII encryption key is unavailable, please retry")
	default:
		return status.Error(codes.Internal, "internal error")
	}
}
