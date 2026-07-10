// Package grpcserver is the gRPC adapter for the ledger. It implements the
// generated LedgerServiceServer by translating protobuf to domain types and
// calling the same ledger services the REST API uses. It holds no business
// rules.
package grpcserver

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/domain"
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
	default:
		return status.Error(codes.Internal, "internal error")
	}
}
