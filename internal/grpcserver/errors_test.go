package grpcserver

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/crypto"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
)

func TestToStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"account not found", domain.ErrAccountNotFound, codes.NotFound},
		{"transaction not found", domain.ErrTransactionNotFound, codes.NotFound},
		{"idempotency key not found", domain.ErrIdempotencyKeyNotFound, codes.NotFound},
		{"duplicate transaction", domain.ErrDuplicateTransaction, codes.AlreadyExists},
		{"idempotency conflict", domain.ErrIdempotencyConflict, codes.AlreadyExists},
		{"unbalanced", domain.ErrUnbalanced, codes.InvalidArgument},
		{"currency mismatch", domain.ErrCurrencyMismatch, codes.InvalidArgument},
		{"too few postings", domain.ErrTooFewPostings, codes.InvalidArgument},
		{"invalid posting", domain.ErrInvalidPosting, codes.InvalidArgument},
		{"invalid account", domain.ErrInvalidAccount, codes.InvalidArgument},
		{"invalid account type", domain.ErrInvalidAccountType, codes.InvalidArgument},
		{"invalid currency", domain.ErrInvalidCurrency, codes.InvalidArgument},
		{"description too long", domain.ErrDescriptionTooLong, codes.InvalidArgument},
		// Task 6.1, audit A9.1 fix: these sentinels had no case here and fell
		// through to the default Internal; both are validation failures, so
		// they map to InvalidArgument like the other too-long cases.
		{"party reference too long", domain.ErrPartyReferenceTooLong, codes.InvalidArgument},
		{"party type too long", domain.ErrPartyTypeTooLong, codes.InvalidArgument},
		{"overflow", domain.ErrOverflow, codes.InvalidArgument},
		{"write conflict", domain.ErrConflict, codes.Unavailable},
		{"policy violation", &domain.PolicyViolationError{Rule: domain.PolicyRuleMaxTransactionAmount, Currency: "USD", Amount: 100, Limit: 50}, codes.FailedPrecondition},
		// Task 6.1, audit A9.1: an explicit screening veto is FailedPrecondition
		// (well-formed request, fails a check, same class as the policy
		// violation above); an ambiguous (non-veto) screening failure fails
		// closed but is Unavailable, the same class as a write conflict.
		{"screening rejected", &ledger.ScreeningRejectedError{Reason: "sanctions match"}, codes.FailedPrecondition},
		{"screening unavailable", ledger.ErrScreeningUnavailable, codes.Unavailable},
		// Task 6.2 fix (audit remediation review, ADR-018): ErrTenantKeyShredded
		// had no case here and fell through to the default Internal; it is a
		// well-formed request that fails an operational precondition on its
		// tenant's key, so it maps to FailedPrecondition like the other typed
		// errors above.
		{"tenant key shredded", crypto.ErrTenantKeyShredded, codes.FailedPrecondition},
		{"unknown", errors.New("boom"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toStatus(tc.err)
			if tc.want == codes.OK {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if status.Code(got) != tc.want {
				t.Errorf("code = %v, want %v", status.Code(got), tc.want)
			}
		})
	}
}

// TestToStatus_ScreeningRejectedMessageNamesReason proves toStatus's
// *ledger.ScreeningRejectedError branch (Task 6.1, audit A9.1) surfaces the
// hook's own reason, not a generic message.
func TestToStatus_ScreeningRejectedMessageNamesReason(t *testing.T) {
	rejected := &ledger.ScreeningRejectedError{Reason: "sanctions list match"}
	got := toStatus(rejected)
	if !strings.Contains(status.Convert(got).Message(), "sanctions list match") {
		t.Errorf("message = %q, want it to contain the hook's reason", status.Convert(got).Message())
	}
}
