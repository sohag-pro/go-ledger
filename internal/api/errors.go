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
	case errors.Is(err, domain.ErrUnbalanced),
		errors.Is(err, domain.ErrCurrencyMismatch),
		errors.Is(err, domain.ErrTooFewPostings),
		errors.Is(err, domain.ErrInvalidPosting),
		errors.Is(err, domain.ErrInvalidAccount),
		errors.Is(err, domain.ErrInvalidAccountType),
		errors.Is(err, domain.ErrInvalidCurrency),
		errors.Is(err, domain.ErrDescriptionTooLong),
		errors.Is(err, domain.ErrOverflow):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return huma.Error500InternalServerError("internal error")
	}
}
