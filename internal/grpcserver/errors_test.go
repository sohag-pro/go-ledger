package grpcserver

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sohag-pro/go-ledger/internal/domain"
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
		{"overflow", domain.ErrOverflow, codes.InvalidArgument},
		{"write conflict", domain.ErrConflict, codes.Unavailable},
		{"policy violation", &domain.PolicyViolationError{Rule: domain.PolicyRuleMaxTransactionAmount, Currency: "USD", Amount: 100, Limit: 50}, codes.FailedPrecondition},
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
