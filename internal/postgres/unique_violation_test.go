package postgres_test

// IsUniqueViolationError (repository.go) is the exported wrapper cmd/server's
// idempotent API key provisioning (ADR-012) uses to treat "a row with this
// key already exists" as success. It is a pure function over an error value,
// so it is tested directly against synthesized pgconn errors, no database
// required, the same synthesized-error technique repository_test.go's own
// serErr() uses for RunInTx's serialization-failure branch.

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sohag-pro/go-ledger/internal/postgres"
)

func TestIsUniqueViolationError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unique violation (23505)",
			err:  &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
			want: true,
		},
		{
			name: "a different pg error code",
			err:  &pgconn.PgError{Code: "23503", Message: "foreign key violation"},
			want: false,
		},
		{
			name: "a plain, non-pg error",
			err:  errors.New("boom"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "wrapped unique violation",
			err:  errors.Join(errors.New("context"), &pgconn.PgError{Code: "23505", Message: "duplicate"}),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := postgres.IsUniqueViolationError(tt.err); got != tt.want {
				t.Errorf("IsUniqueViolationError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
