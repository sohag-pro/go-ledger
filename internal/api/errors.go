package api

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/domain"
)

// toHumaErr maps a domain error to a huma status error, which huma renders as an
// RFC 7807 application/problem+json response. Validation and invariant failures
// are client errors (422), missing resources are 404, a duplicate id is 409, a
// transient write conflict is 503, and anything unrecognized is a 500 that does
// not leak internals. FX conversion failures (bad rate, bad spread, dust, no
// rate for the pair, self or same-currency conversion, non-positive source
// amount) are all client errors too, so they map to 422 like the other
// validation/invariant failures (ADR-014); a cross-tenant or unknown account on
// either leg still falls through to the existing ErrAccountNotFound case above.
//
// The admin surface (Task 2.2b, internal/admin) reuses this same mapping
// rather than defining its own: a *domain.TenantNotActiveError (issuing or
// rotating a key into a suspended or closed tenant) is checked first, via
// errors.As, so its Reason() names the exact status the way the auth
// resolver's identical 403 already does (ADR-015); every other admin error
// (missing tenant, missing key, invalid tenant status, invalid scopes) falls
// through to the switch below alongside the rest of the domain's errors.
func toHumaErr(err error) error {
	var tenantErr *domain.TenantNotActiveError
	if errors.As(err, &tenantErr) {
		return huma.Error403Forbidden(tenantErr.Reason())
	}
	// A *domain.PolicyViolationError (Task 2.4b, audit A3.4) is checked via
	// errors.As, like TenantNotActiveError above, so its Error() message
	// (naming the exact rule, currency, and amount/limit) reaches the client
	// instead of a generic "policy violation" the switch below would give a
	// bare errors.Is(err, domain.ErrPolicyViolation) match.
	var policyErr *domain.PolicyViolationError
	if errors.As(err, &policyErr) {
		return huma.Error422UnprocessableEntity(policyErr.Error())
	}

	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrTenantNotFound):
		return huma.Error404NotFound("tenant not found")
	case errors.Is(err, domain.ErrAPIKeyNotFound):
		return huma.Error404NotFound("api key not found")
	case errors.Is(err, domain.ErrInvalidTenant):
		return huma.Error422UnprocessableEntity("invalid tenant name or status")
	case errors.Is(err, admin.ErrInvalidScopes):
		return huma.Error422UnprocessableEntity("at least one valid scope (read, post, admin) is required")
	case errors.Is(err, domain.ErrInvalidTenantPolicy):
		return huma.Error422UnprocessableEntity("invalid tenant policy: amounts must be non-negative and allowed_currencies must be well-formed three-letter codes")
	case errors.Is(err, domain.ErrAccountNotFound):
		return huma.Error404NotFound("account not found")
	case errors.Is(err, domain.ErrTransactionNotFound):
		return huma.Error404NotFound("transaction not found")
	case errors.Is(err, domain.ErrDuplicateTransaction):
		return huma.Error409Conflict("transaction already exists")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return huma.Error409Conflict("idempotency key was reused with a different request body")
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
	case errors.Is(err, domain.ErrConversionDust):
		return huma.Error422UnprocessableEntity("conversion amount rounds to zero in the destination currency")
	case errors.Is(err, domain.ErrNonPositiveRate):
		return huma.Error422UnprocessableEntity("fx rate must be positive")
	case errors.Is(err, domain.ErrInvalidSpread):
		return huma.Error422UnprocessableEntity("fx spread is out of range")
	case errors.Is(err, domain.ErrFXRateNotFound):
		return huma.Error422UnprocessableEntity("no fx rate is configured for this currency pair")
	case errors.Is(err, domain.ErrNonPositiveConvertAmount):
		return huma.Error422UnprocessableEntity("source_amount must be positive")
	case errors.Is(err, domain.ErrSelfConversion):
		return huma.Error422UnprocessableEntity("from_account and to_account must differ")
	case errors.Is(err, domain.ErrSameCurrencyConversion):
		return huma.Error422UnprocessableEntity("from_account and to_account must have different currencies")
	default:
		return huma.Error500InternalServerError("internal error")
	}
}
