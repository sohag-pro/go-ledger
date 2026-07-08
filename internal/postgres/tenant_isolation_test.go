package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestIdempotencyKeyTenantIsolation proves a key inserted under one tenant is
// invisible to a lookup under a different tenant, even with the exact same key
// string, mirroring TestTenantIsolation in repository_test.go.
func TestIdempotencyKeyTenantIsolation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	owner := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, owner)

	err := repo.RunInTx(ctx, owner, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, owner, "shared-key", "fp-1", txnID)
	})
	if err != nil {
		t.Fatalf("insert idempotency key: %v", err)
	}

	other := uuid.NewString()
	if _, err := repo.GetIdempotencyKey(ctx, other, "shared-key"); !errors.Is(err, domain.ErrIdempotencyKeyNotFound) {
		t.Errorf("cross-tenant idempotency key lookup: got %v, want ErrIdempotencyKeyNotFound", err)
	}
}

// TestListAuditByTransactionTenantIsolation proves audit rows written under one
// tenant do not surface for the same transaction id queried under another tenant.
func TestListAuditByTransactionTenantIsolation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	owner := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, owner)

	err := repo.RunInTx(ctx, owner, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAudit(ctx, owner, domain.AuditEntry{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txnID,
			Actor:         owner,
			After:         []byte(`{"id":"` + txnID + `"}`),
		})
	})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}

	other := uuid.NewString()
	got, err := repo.ListAuditByTransaction(ctx, other, txnID)
	if err != nil {
		t.Fatalf("list by txn under other tenant: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cross-tenant audit-by-transaction: got %d rows, want 0", len(got))
	}
}

// TestListAuditByAccountTenantIsolation proves audit rows tied to one tenant's
// account do not surface when the same account id is queried under another
// tenant (accounts, and therefore their postings' audit trail, never cross
// tenants).
func TestListAuditByAccountTenantIsolation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	owner := uuid.NewString()
	txnID, debit, _ := seedTxn(t, repo, owner)

	err := repo.RunInTx(ctx, owner, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAudit(ctx, owner, domain.AuditEntry{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txnID,
			Actor:         owner,
			After:         []byte(`{"id":"` + txnID + `"}`),
		})
	})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}

	other := uuid.NewString()
	got, err := repo.ListAuditByAccount(ctx, other, debit, nil, 50)
	if err != nil {
		t.Fatalf("list by account under other tenant: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("cross-tenant audit-by-account: got %d rows, want 0", len(got))
	}
}
