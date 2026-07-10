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
// port too (Task 6.2, audit A9.3; versioned per ADR-018), alongside
// domain.Repository: cmd/server hands the same *Repository value to both
// NewCipher and postgres.NewRepository.
var _ crypto.KeyStore = (*Repository)(nil)

// CurrentTenantDEK implements crypto.KeyStore. It runs through withTenant
// (RLS GUC set to tenantID, migration 0027), the same tenant scoping every
// other request-path read/write in this file uses: a request that forgot to
// check its own tenant boundary elsewhere still cannot read another
// tenant's key.
func (r *Repository) CurrentTenantDEK(ctx context.Context, tenantID string) (wrappedDEK []byte, version int, shredded, found bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, 0, false, false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetCurrentCryptoKeyRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetCurrentCryptoKey(ctx, tid)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, false, nil
	}
	if err != nil {
		return nil, 0, false, false, fmt.Errorf("postgres: get current crypto key: %w", err)
	}
	if row.ShreddedAt.Valid {
		return nil, int(row.Version), true, true, nil
	}
	return row.WrappedDek, int(row.Version), false, true, nil
}

// MintTenantDEKVersion implements crypto.KeyStore. Like CurrentTenantDEK, it
// runs through withTenant, even though the mint statement itself also takes
// a per-tenant advisory lock (see queries/crypto_keys.sql's own comment):
// the RLS scoping and the advisory-lock serialization guard against two
// different things (a forgotten tenant boundary vs. a mint/shred race) and
// are both applied uniformly with every other write in this file.
func (r *Repository) MintTenantDEKVersion(ctx context.Context, tenantID string, version int, candidateWrappedDEK []byte) (wrappedDEK []byte, shredded bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.MintCryptoKeyVersionRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.MintCryptoKeyVersion(ctx, sqlc.MintCryptoKeyVersionParams{
			TenantID:   tid,
			Version:    int32(version), //nolint:gosec // version is a small, monotonically increasing per-tenant counter, never near int32's range
			WrappedDek: candidateWrappedDEK,
		})
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("postgres: mint crypto key version: %w", err)
	}
	if row.ShreddedAt.Valid {
		return nil, true, nil
	}
	return row.WrappedDek, false, nil
}

// TenantDEKVersion implements crypto.KeyStore. Unlike CurrentTenantDEK it
// never creates a row and never looks past the exact version requested:
// found is false when tenantID has no crypto_keys row at that version at
// all.
func (r *Repository) TenantDEKVersion(ctx context.Context, tenantID string, version int) (wrappedDEK []byte, shredded, found bool, err error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, false, false, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var row sqlc.GetCryptoKeyVersionRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetCryptoKeyVersion(ctx, sqlc.GetCryptoKeyVersionParams{
			TenantID: tid,
			Version:  int32(version), //nolint:gosec // see MintTenantDEKVersion
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("postgres: get crypto key version: %w", err)
	}
	if row.ShreddedAt.Valid {
		return nil, true, true, nil
	}
	return row.WrappedDek, false, true, nil
}

// ShredTenantCryptoKey implements domain.Repository.ShredTenantCryptoKey
// (Task 6.2, audit A9.3; versioned per ADR-018): see that interface method's
// doc comment for the full crypto-shredding argument. It destroys only
// tenantID's CURRENT (highest-version) key: internal/crypto.Cipher's next
// Encrypt call for this tenant mints a fresh, forward version rather than
// failing closed forever, so the tenant keeps operating after an erasure
// request. Run through withTenant like every other tenant-scoped write in
// this file, even though this is an admin-only operation
// (internal/admin.Service.ShredTenantPII is its only caller today): the RLS
// backstop applies uniformly, not just to the ordinary request path.
func (r *Repository) ShredTenantCryptoKey(ctx context.Context, tenantID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	if err := r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		return q.ShredCurrentCryptoKey(ctx, tid)
	}); err != nil {
		return fmt.Errorf("postgres: shred tenant crypto key: %w", err)
	}
	return nil
}
