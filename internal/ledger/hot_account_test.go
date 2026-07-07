package ledger_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestPostConcurrentSameAccount is the worst case for SERIALIZABLE: every
// goroutine posts against the exact same two accounts, so every transaction
// conflicts with every other in flight. Unlike TestPostConcurrentStress (spread
// across 100 accounts), here there is no way to avoid contention.
//
// Under this much contention on two rows, some posts may legitimately exhaust
// the service's bounded retries (see maxPostAttempts in internal/postgres) and
// come back as domain.ErrConflict, mapped to HTTP 503 at the API layer. That is
// a valid, expected outcome, not corruption, so this test does NOT assert that
// every post succeeds. It asserts the ledger stays correct regardless of which
// posts won the race:
//   - every post either succeeds or fails with domain.ErrConflict, nothing else;
//   - the two accounts' balances are exact negatives of each other and net to
//     zero (no money created or destroyed);
//   - the number of committed transactions equals the number of successful
//     posts (no phantom or duplicate postings survived the retries).
func TestPostConcurrentSameAccount(t *testing.T) {
	const goroutines = 60

	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()
	tenant := uuid.NewString()

	a := &domain.Account{Name: "Hot A", Type: domain.Asset, Currency: "USD"}
	b := &domain.Account{Name: "Hot B", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, a); err != nil {
		t.Fatalf("create account a: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, b); err != nil {
		t.Fatalf("create account b: %v", err)
	}

	var (
		successes atomic.Int64
		conflicts atomic.Int64
		wg        sync.WaitGroup
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			// Alternate direction so the transaction still balances regardless of
			// which way money moves; the amount just needs to be nonzero.
			amt := int64(seed%7 + 1)
			debit, _ := domain.NewMoney(amt, "USD")
			credit, _ := domain.NewMoney(-amt, "USD")
			from, to := a.ID, b.ID
			if seed%2 == 0 {
				from, to = to, from
			}
			txn := &domain.Transaction{Postings: []domain.Posting{
				{AccountID: from, Amount: debit},
				{AccountID: to, Amount: credit},
			}}
			_, err := svc.Post(ctx, tenant, txn, nil)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, domain.ErrConflict):
				conflicts.Add(1)
			default:
				t.Errorf("post returned unexpected error type: %v", err)
			}
		}(g)
	}
	wg.Wait()

	t.Logf("hot-account contention: %d successes, %d conflicts (of %d posts)",
		successes.Load(), conflicts.Load(), goroutines)

	if successes.Load()+conflicts.Load() != goroutines {
		t.Fatalf("successes(%d) + conflicts(%d) != goroutines(%d); an unexpected error type slipped through",
			successes.Load(), conflicts.Load(), goroutines)
	}

	balA, err := repo.Balance(ctx, tenant, a.ID)
	if err != nil {
		t.Fatalf("balance a: %v", err)
	}
	balB, err := repo.Balance(ctx, tenant, b.ID)
	if err != nil {
		t.Fatalf("balance b: %v", err)
	}
	if balA.Amount() != -balB.Amount() {
		t.Errorf("balances are not exact negatives: a=%d b=%d", balA.Amount(), balB.Amount())
	}
	if balA.Amount()+balB.Amount() != 0 {
		t.Errorf("ledger does not net to zero: a=%d b=%d sum=%d", balA.Amount(), balB.Amount(), balA.Amount()+balB.Amount())
	}

	// Every successful post created one transaction touching account a; count
	// them back out via the statement to confirm no phantom or duplicate
	// postings survived retries.
	entries, err := repo.Statement(ctx, tenant, a.ID, "USD", nil, goroutines+10)
	if err != nil {
		t.Fatalf("statement a: %v", err)
	}
	if int64(len(entries)) != successes.Load() {
		t.Errorf("committed postings on account a = %d, want %d (successful posts)", len(entries), successes.Load())
	}
}
