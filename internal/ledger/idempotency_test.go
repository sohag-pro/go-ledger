package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

func mkTxn(t *testing.T, debit, credit string) *domain.Transaction {
	t.Helper()
	d, _ := domain.NewMoney(250, "USD")
	c, _ := domain.NewMoney(-250, "USD")
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: d},
		{AccountID: credit, Amount: c},
	}}
}

func TestPostIdempotentHammer(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, nil, nil)
	ctx := context.Background()
	tenant := uuid.NewString()

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("create debit: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, credit); err != nil {
		t.Fatalf("create credit: %v", err)
	}

	const n = 100
	idem := &domain.Idempotency{Key: "same-key"}
	var wg sync.WaitGroup
	ids := make([]string, n)
	replays := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			txn := mkTxn(t, debit.ID, credit.ID)
			replayed, err := svc.Post(ctx, tenant, txn, idem)
			ids[i], replays[i], errs[i] = txn.ID, replayed, err
		}(i)
	}
	wg.Wait()

	// Every call succeeded, all returned the same transaction id.
	var first string
	replayCount := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if first == "" {
			first = ids[i]
		} else if ids[i] != first {
			t.Fatalf("call %d returned id %s, want %s", i, ids[i], first)
		}
		if replays[i] {
			replayCount++
		}
	}
	if replayCount != n-1 {
		t.Errorf("replay count = %d, want %d", replayCount, n-1)
	}

	// Exactly one audit row for the one transaction.
	audit, err := repo.ListAuditByTransaction(ctx, tenant, first)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(audit) != 1 {
		t.Errorf("audit rows = %d, want 1", len(audit))
	}
}

func TestPostIdempotentConflict(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, nil, nil)
	ctx := context.Background()
	tenant := uuid.NewString()

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	_ = repo.CreateAccount(ctx, tenant, debit)
	_ = repo.CreateAccount(ctx, tenant, credit)

	idem := &domain.Idempotency{Key: "k"}
	if _, err := svc.Post(ctx, tenant, mkTxn(t, debit.ID, credit.ID), idem); err != nil {
		t.Fatalf("first post: %v", err)
	}
	// Same key, different body (amount): conflict.
	d, _ := domain.NewMoney(999, "USD")
	c, _ := domain.NewMoney(-999, "USD")
	other := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit.ID, Amount: d},
		{AccountID: credit.ID, Amount: c},
	}}
	if _, err := svc.Post(ctx, tenant, other, idem); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("mismatched body: got %v, want ErrIdempotencyConflict", err)
	}
}
