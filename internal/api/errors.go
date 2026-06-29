package api

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// toHumaErr maps a domain error to a huma status error, which huma renders as an
// RFC 7807 application/problem+json response. Validation and invariant failures
// are client errors (422), missing resources are 404, a duplicate id is 409, a
// transient write conflict is 503, and anything unrecognized is a 500 that does
// not leak internals.
func toHumaErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAccountNotFound):
		return huma.Error404NotFound("account not found")
	case errors.Is(err, domain.ErrTransactionNotFound):
		return huma.Error404NotFound("transaction not found")
	case errors.Is(err, domain.ErrDuplicateTransaction):
		return huma.Error409Conflict("transaction already exists")
	case errors.Is(err, domain.ErrConflict):
		return huma.Error503ServiceUnavailable("write conflict, please retry")
	case errors.Is(err, domain.ErrUnbalanced):
		return huma.Error422UnprocessableEntity("transaction postings must sum to zero")
	case errors.Is(err, domain.ErrCurrencyMismatch):
		return huma.Error422UnprocessableEntity("every posting must be in the transaction's currency, and that currency must match the account's currency")
	case errors.Is(err, domain.ErrTooFewPostings):
		return huma.Error422UnprocessableEntity("a transaction needs at least two postings")
	case errors.Is(err, domain.ErrInvalidPosting):
		return huma.Error422UnprocessableEntity("a posting is missing its account id")
	case errors.Is(err, domain.ErrInvalidAccount):
		return huma.Error422UnprocessableEntity("account is missing a required field")
	case errors.Is(err, domain.ErrInvalidAccountType):
		return huma.Error422UnprocessableEntity("invalid account type")
	case errors.Is(err, domain.ErrInvalidCurrency):
		return huma.Error422UnprocessableEntity("invalid currency code (expected three uppercase letters)")
	case errors.Is(err, domain.ErrDescriptionTooLong):
		return huma.Error422UnprocessableEntity("posting description is too long")
	case errors.Is(err, domain.ErrOverflow):
		return huma.Error422UnprocessableEntity("amount is out of range")
	default:
		return huma.Error500InternalServerError("internal error")
	}
}
