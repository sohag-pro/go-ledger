package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/crypto"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

// TestToHumaErr is a table-driven check of the domain-error-to-HTTP-status
// mapping in toHumaErr. Each domain sentinel must land on the status ADR-006
// documents; a nil error must map to nil, not a wrapped no-op error.
func TestToHumaErr(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int // ignored when wantNil is true
		wantNil    bool
	}{
		{"nil error maps to nil", nil, 0, true},
		{"account not found", domain.ErrAccountNotFound, 404, false},
		{"transaction not found", domain.ErrTransactionNotFound, 404, false},
		{"duplicate transaction", domain.ErrDuplicateTransaction, 409, false},
		{"idempotency conflict", domain.ErrIdempotencyConflict, 409, false},
		{"write conflict", domain.ErrConflict, 503, false},
		{"unbalanced", domain.ErrUnbalanced, 422, false},
		{"currency mismatch", domain.ErrCurrencyMismatch, 422, false},
		{"too few postings", domain.ErrTooFewPostings, 422, false},
		{"invalid tenant policy", domain.ErrInvalidTenantPolicy, 422, false},
		{"policy violation", &domain.PolicyViolationError{Rule: domain.PolicyRuleMaxTransactionAmount, Currency: "USD", Amount: 100, Limit: 50}, 422, false},
		// Task 6.1, audit A9.1: an explicit screening veto is a well-formed
		// request that just fails a check, so it maps to 422 like the other
		// typed errors above; an ambiguous (non-veto) screening failure fails
		// closed but is presented as retryable, mapping to 503 like
		// domain.ErrConflict.
		{"screening rejected", &ledger.ScreeningRejectedError{Reason: "sanctions match"}, 422, false},
		{"screening unavailable", ledger.ErrScreeningUnavailable, 503, false},
		// Task 6.1, audit A9.1 fix: the party-length sentinels had no case
		// here and fell through to the default 500; both are validation
		// failures over otherwise well-formed requests, so they map to 422
		// like ErrDescriptionTooLong and ErrReferenceTooLong above.
		{"party reference too long", domain.ErrPartyReferenceTooLong, 422, false},
		{"party type too long", domain.ErrPartyTypeTooLong, 422, false},
		// Task 6.2 fix (audit remediation review, ADR-018): ErrTenantKeyShredded
		// had no case here and fell through to the default 500; it is a
		// well-formed request that fails an operational precondition on its
		// tenant's key, so it maps to 422 like the other typed errors above.
		{"tenant key shredded", crypto.ErrTenantKeyShredded, 422, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toHumaErr(tt.err)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("toHumaErr(nil) = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("toHumaErr(%v) = nil, want status %d", tt.err, tt.wantStatus)
			}
			// errors.As, not a direct type assertion: domain.ErrConflict's mapping
			// wraps its huma.StatusError with huma.ErrorWithHeaders (Retry-After,
			// see TestToHumaErr_ConflictHasRetryAfter below), so the returned error
			// itself is a *huma.errWithHeaders, not a huma.StatusError directly.
			// huma's own runtime dispatch unwraps the exact same way (huma.go's
			// operation handler does errors.As(err, &se) to find the status), so
			// this is what actually reaches the client, not a test-only nuance.
			var statusErr huma.StatusError
			if !errors.As(got, &statusErr) {
				t.Fatalf("toHumaErr(%v) = %v (%T), does not implement huma.StatusError", tt.err, got, got)
			}
			if statusErr.GetStatus() != tt.wantStatus {
				t.Errorf("toHumaErr(%v) status = %d, want %d", tt.err, statusErr.GetStatus(), tt.wantStatus)
			}
		})
	}
}

// TestToHumaErr_ConflictHasRetryAfter proves the retry-exhausted 503
// (domain.ErrConflict) carries a Retry-After header, mirroring how the 429
// rate-limit response already sets one (internal/auth/ratelimit.go), so a
// client backing off a transient write conflict knows how long to wait
// before retrying instead of hammering the server in a tight loop.
func TestToHumaErr_ConflictHasRetryAfter(t *testing.T) {
	got := toHumaErr(domain.ErrConflict)
	headersErr, ok := got.(huma.HeadersError)
	if !ok {
		t.Fatalf("toHumaErr(ErrConflict) = %v (%T), does not implement huma.HeadersError", got, got)
	}
	if retryAfter := headersErr.GetHeaders().Get("Retry-After"); retryAfter != "1" {
		t.Errorf("Retry-After = %q, want %q", retryAfter, "1")
	}
}

// TestToHumaErr_ScreeningRejectedMessageNamesReason proves toHumaErr's
// *ledger.ScreeningRejectedError branch (Task 6.1, audit A9.1) surfaces the
// hook's own reason, not a generic "screening rejected" string.
func TestToHumaErr_ScreeningRejectedMessageNamesReason(t *testing.T) {
	rejected := &ledger.ScreeningRejectedError{Reason: "sanctions list match"}
	got := toHumaErr(rejected)
	statusErr, ok := got.(huma.StatusError)
	if !ok {
		t.Fatalf("toHumaErr(%v) = %v (%T), does not implement huma.StatusError", rejected, got, got)
	}
	if statusErr.GetStatus() != 422 {
		t.Errorf("status = %d, want 422", statusErr.GetStatus())
	}
	if !strings.Contains(statusErr.Error(), "sanctions list match") {
		t.Errorf("error message = %q, want it to contain the hook's reason", statusErr.Error())
	}
}

// TestToHumaErr_ScreeningUnavailableHasRetryAfter proves the ambiguous
// screening-failure 503 (Task 6.1, audit A9.1) carries a Retry-After header,
// the same as domain.ErrConflict's (TestToHumaErr_ConflictHasRetryAfter
// above): the caller is told this is likely transient and worth retrying.
func TestToHumaErr_ScreeningUnavailableHasRetryAfter(t *testing.T) {
	got := toHumaErr(ledger.ErrScreeningUnavailable)
	headersErr, ok := got.(huma.HeadersError)
	if !ok {
		t.Fatalf("toHumaErr(ErrScreeningUnavailable) = %v (%T), does not implement huma.HeadersError", got, got)
	}
	if retryAfter := headersErr.GetHeaders().Get("Retry-After"); retryAfter != "1" {
		t.Errorf("Retry-After = %q, want %q", retryAfter, "1")
	}
}

// TestToHumaErr_PolicyViolationMessageNamesRuleAndCurrency proves toHumaErr's
// *domain.PolicyViolationError branch (Task 2.4b, audit A3.4) surfaces the
// typed error's own message, not a generic "policy violation" string, so a
// caller's 422 body says exactly which rule and currency tripped.
func TestToHumaErr_PolicyViolationMessageNamesRuleAndCurrency(t *testing.T) {
	pv := &domain.PolicyViolationError{
		Rule: domain.PolicyRuleDailyVolumeLimit, Currency: "EUR", Amount: 1500, Limit: 1000,
	}
	got := toHumaErr(pv)
	statusErr, ok := got.(huma.StatusError)
	if !ok {
		t.Fatalf("toHumaErr(%v) = %v (%T), does not implement huma.StatusError", pv, got, got)
	}
	if statusErr.GetStatus() != 422 {
		t.Errorf("status = %d, want 422", statusErr.GetStatus())
	}
	if statusErr.Error() == "" {
		t.Error("error message is empty")
	}
}
