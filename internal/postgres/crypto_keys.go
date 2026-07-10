package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sohag-pro/go-ledger/internal/crypto"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// compile-time check that Repository satisfies internal/crypto's persistence
// port too (Task 6.2, audit A9.3), alongside domain.Repository: cmd/server
// hands the same *Repository value to both postgres.NewRepository and
// crypto.NewCipher.
var _ crypto.KeyStore = (*Repository)(nil)

// GetOrCreateWrappedDEK implements crypto.KeyStore. It runs through
// withTenant (RLS GUC set to tenantID, migration 0027), the same tenant
// scoping every other request-path read/write in this file uses: a request
// that forgot to check its own tenant boundary elsewhere still cannot read or
// create another tenant's key.
func (r *Repository) GetOrCreateWrappedDEK(ctx context.Context, tenantID string, candidateWrappedDEK []byte) (wrappedDEK []byte, shredded bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetOrCreateCryptoKeyRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetOrCreateCryptoKey(ctx, sqlc.GetOrCreateCryptoKeyParams{TenantID: tid, WrappedDek: candidateWrappedDEK})
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("postgres: get or create crypto key: %w", err)
	}
	if row.ShreddedAt.Valid {
		return nil, true, nil
	}
	return row.WrappedDek, false, nil
}

// GetWrappedDEK implements crypto.KeyStore. Unlike GetOrCreateWrappedDEK it
// never creates a row: found is false when tenantID has no crypto_keys row
// at all.
func (r *Repository) GetWrappedDEK(ctx context.Context, tenantID string) (wrappedDEK []byte, shredded, found bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, false, false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetCryptoKeyRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetCryptoKey(ctx, tid)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("postgres: get crypto key: %w", err)
	}
	if row.ShreddedAt.Valid {
		return nil, true, true, nil
	}
	return row.WrappedDek, false, true, nil
}

// ShredTenantCryptoKey implements domain.Repository.ShredTenantCryptoKey
// (Task 6.2, audit A9.3): see that interface method's doc comment for the
// full crypto-shredding argument. Run through withTenant like every other
// tenant-scoped write in this file, even though this is an admin-only
// operation (internal/admin.Service.ShredTenantPII is its only caller
// today): the RLS backstop applies uniformly, not just to the ordinary
// request path.
func (r *Repository) ShredTenantCryptoKey(ctx context.Context, tenantID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if err := r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		return q.ShredCryptoKey(ctx, tid)
	}); err != nil {
		return fmt.Errorf("postgres: shred tenant crypto key: %w", err)
	}
	return nil
}
