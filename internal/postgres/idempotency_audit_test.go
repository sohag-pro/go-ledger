package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// seedTxn creates two accounts and posts one balanced transaction, returning the
// transaction id and the two account ids.
func seedTxn(t *testing.T, repo *postgres.Repository, tenant string) (txnID, debit, credit string) { //nolint:unparam // credit is part of the helper's shape for future callers even though no test currently reads it
	t.Helper()
	ctx := context.Background()
	d := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	c := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, d); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, c); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: d.ID, Amount: money(t, 500, "USD")},
		{AccountID: c.ID, Amount: money(t, -500, "USD")},
	}}
	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	return txn.ID, d.ID, c.ID
}

func TestIdempotencyKeyInsertAndDuplicate(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, tenant)

	// First insert of the key succeeds inside a Tx.
	err := repo.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "key-1", "fp-1", txnID)
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Second insert of the same key returns ErrDuplicateIdempotencyKey.
	err = repo.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "key-1", "fp-1", txnID)
	})
	if !errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		t.Fatalf("duplicate insert: got %v, want ErrDuplicateIdempotencyKey", err)
	}

	// The stored record round-trips.
	rec, err := repo.GetIdempotencyKey(ctx, tenant, "key-1")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if rec.Fingerprint != "fp-1" || rec.TransactionID != txnID {
		t.Errorf("record = %+v, want fingerprint fp-1 and txn %s", rec, txnID)
	}

	// A missing key is a typed not-found.
	if _, err := repo.GetIdempotencyKey(ctx, tenant, "nope"); !errors.Is(err, domain.ErrIdempotencyKeyNotFound) {
		t.Errorf("missing key: got %v, want ErrIdempotencyKeyNotFound", err)
	}
}

func TestAuditAppendAndQuery(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, debit, _ := seedTxn(t, repo, tenant)

	err := repo.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAudit(ctx, tenant, domain.AuditEntry{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txnID,
			Actor:         tenant,
			After:         []byte(`{"id":"` + txnID + `"}`),
		})
	})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}

	byTxn, err := repo.ListAuditByTransaction(ctx, tenant, txnID)
	if err != nil {
		t.Fatalf("list by txn: %v", err)
	}
	if len(byTxn) != 1 || byTxn[0].Action != domain.ActionTransactionCreated {
		t.Fatalf("by txn = %+v, want one transaction.created row", byTxn)
	}
	if byTxn[0].ID == "" || byTxn[0].CreatedAt.IsZero() {
		t.Error("audit row missing generated id or created_at")
	}

	byAcct, err := repo.ListAuditByAccount(ctx, tenant, debit, nil, 50)
	if err != nil {
		t.Fatalf("list by account: %v", err)
	}
	if len(byAcct) != 1 || byAcct[0].TransactionID != txnID {
		t.Fatalf("by account = %+v, want the txn's audit row", byAcct)
	}
}
