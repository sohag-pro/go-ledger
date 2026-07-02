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
// anything unrecognized is Internal without leaking internals.
func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAccountNotFound):
		return status.Error(codes.NotFound, "account not found")
	case errors.Is(err, domain.ErrTransactionNotFound):
		return status.Error(codes.NotFound, "transaction not found")
	case errors.Is(err, domain.ErrIdempotencyKeyNotFound):
		return status.Error(codes.NotFound, "idempotency key not found")
	case errors.Is(err, domain.ErrDuplicateTransaction):
		return status.Error(codes.AlreadyExists, "transaction already exists")
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
	case errors.Is(err, domain.ErrOverflow):
		return status.Error(codes.InvalidArgument, "amount is out of range")
	default:
		return status.Error(codes.Internal, "internal error")
	}
}
