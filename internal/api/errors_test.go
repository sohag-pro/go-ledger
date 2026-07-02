package api

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sohag-pro/go-ledger/internal/domain"
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
			statusErr, ok := got.(huma.StatusError)
			if !ok {
				t.Fatalf("toHumaErr(%v) = %v (%T), does not implement huma.StatusError", tt.err, got, got)
			}
			if statusErr.GetStatus() != tt.wantStatus {
				t.Errorf("toHumaErr(%v) status = %d, want %d", tt.err, statusErr.GetStatus(), tt.wantStatus)
			}
		})
	}
}
