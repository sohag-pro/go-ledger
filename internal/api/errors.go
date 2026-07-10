package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// conflictRetryAfterSeconds is the fixed Retry-After value (in seconds) sent
// with the retry-exhausted 503 for domain.ErrConflict. Unlike the 429 rate
// limiter's Retry-After (internal/auth/ratelimit.go's retryAfterSeconds),
// which computes its value from the limiter's actual refill rate, a write
// conflict has nothing comparable to compute from: the per-tenant
// serialization that produced it is expected to clear well within a second, so
// a small fixed value gives an honest, cheap-to-retry hint without implying a
// precision the server cannot back up.
const conflictRetryAfterSeconds = 1

// toHumaErr maps a domain error to a huma status error, which huma renders as an
// RFC 7807 application/problem+json response. Validation and invariant failures
// are client errors (422), missing resources are 404, a duplicate id is 409, a
// transient write conflict is 503 (with a Retry-After header, conflictRetryAfterSeconds
// below, so a client knows to back off), and anything unrecognized is a 500 that does
// not leak internals. FX conversion failures (bad rate, bad spread, dust, no
// rate for the pair, self or same-currency conversion, non-positive source
// amount) are all client errors too, so they map to 422 like the other
// validation/invariant failures (ADR-014); a cross-tenant or unknown account on
// either leg still falls through to the existing ErrAccountNotFound case above.
//
// A duplicate external reference (Task 4.3, audit A1.3,
// domain.ErrDuplicateReference) also maps to 409, like ErrDuplicateTransaction
// and ErrIdempotencyConflict, but is kept as its own case: it is a different
// request that happens to reuse someone else's reference, not a retried
// request replaying under the same idempotency key.
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
	// *domain.AccountNotActiveError and *domain.MinBalanceBreachError (Task
	// 5.5, audit A1.5) are checked the same way, before the switch below, so
	// each names the exact account and status/floor involved instead of a
	// generic bare errors.Is match. Both map to 422, the same class as a
	// tripped TenantPolicy guardrail: the request is otherwise well-formed,
	// it just fails a constraint the touched account carries.
	var accountNotActiveErr *domain.AccountNotActiveError
	if errors.As(err, &accountNotActiveErr) {
		return huma.Error422UnprocessableEntity(accountNotActiveErr.Error())
	}
	var minBalanceErr *domain.MinBalanceBreachError
	if errors.As(err, &minBalanceErr) {
		return huma.Error422UnprocessableEntity(minBalanceErr.Error())
	}
	// *ledger.ScreeningRejectedError (Task 6.1, audit A9.1) is checked the
	// same way: an external screening/compliance hook explicitly vetoed the
	// post. Like the errors above, this is a well-formed request that just
	// fails a check, so it maps to 422 with the hook's own reason. It is
	// checked BEFORE errors.Is(err, ledger.ErrScreeningUnavailable) below:
	// ScreeningRejectedError also wraps ledger.ErrScreeningRejected, a
	// distinct sentinel from ErrScreeningUnavailable, so the two can never
	// both match the same error and this ordering is purely for readability
	// (matching the errors.As-then-switch layout the rest of this function
	// already uses).
	var screeningRejectedErr *ledger.ScreeningRejectedError
	if errors.As(err, &screeningRejectedErr) {
		return huma.Error422UnprocessableEntity(screeningRejectedErr.Error())
	}

	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrTenantNotFound):
		return huma.Error404NotFound("tenant not found")
	case errors.Is(err, domain.ErrAPIKeyNotFound):
		return huma.Error404NotFound("api key not found")
	case errors.Is(err, domain.ErrWebhookSubscriptionNotFound):
		return huma.Error404NotFound("webhook subscription not found")
	case errors.Is(err, domain.ErrInvalidWebhookURL):
		return huma.Error422UnprocessableEntity("webhook url must be an absolute http or https URL")
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
	case errors.Is(err, domain.ErrCannotReverseReversal):
		return huma.Error422UnprocessableEntity("cannot reverse a transaction that is itself a reversal")
	case errors.Is(err, domain.ErrDuplicateTransaction):
		return huma.Error409Conflict("transaction already exists")
	case errors.Is(err, domain.ErrDuplicateReference):
		return huma.Error409Conflict("reference already exists for this tenant")
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return huma.Error409Conflict("idempotency key was reused with a different request body")
	case errors.Is(err, domain.ErrConflict):
		return huma.ErrorWithHeaders(
			huma.Error503ServiceUnavailable("write conflict, please retry"),
			http.Header{"Retry-After": []string{strconv.Itoa(conflictRetryAfterSeconds)}},
		)
	// ledger.ErrScreeningUnavailable (Task 6.1, audit A9.1) is an AMBIGUOUS
	// screening failure (a timeout, a dropped connection, anything that is
	// not an explicit veto): fail closed, the post is rejected, but unlike a
	// definite veto (ScreeningRejectedError above) the caller is told this
	// is likely transient and worth retrying, the same class of 503 as a
	// write-conflict retry-exhaustion.
	case errors.Is(err, ledger.ErrScreeningUnavailable):
		return huma.ErrorWithHeaders(
			huma.Error503ServiceUnavailable("screening system unavailable, please retry"),
			http.Header{"Retry-After": []string{strconv.Itoa(conflictRetryAfterSeconds)}},
		)
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
	case errors.Is(err, domain.ErrInvalidReference):
		return huma.Error422UnprocessableEntity("reference must not be empty when present")
	case errors.Is(err, domain.ErrReferenceTooLong):
		return huma.Error422UnprocessableEntity("reference is too long")
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
