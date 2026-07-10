package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
	if err := repo.CreateTenant(ctx, tenant, "idempotency test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

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

	// Post only writes an audit_outbox row (ADR-017); drain the chainer so
	// there is an audit_log row to check.
	drainChainer(t, pool, tenant)

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
	if err := repo.CreateTenant(ctx, tenant, "idempotency test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

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

// TestPostIdempotencyKeyExpires proves the TTL end to end through the service
// (Task 4.5, audit A1.4): a service constructed with a tiny
// WithIdempotencyTTL replays a retry submitted before expiry, but once the
// key has expired, a request reusing the same key and a DIFFERENT body is no
// longer a conflict: the expired key is treated as absent, so it proceeds as
// a brand-new post instead of returning ErrIdempotencyConflict.
func TestPostIdempotencyKeyExpires(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	// tinyTTL needs a real margin, not a real "tiny" one: the "still live"
	// assertion below has to survive several sequential Postgres round trips
	// (tenant-policy fetch, the first post's commit, then the retry's
	// precheck plus its replay GetIdempotencyKey and GetTransaction) before
	// the ttl elapses. 50ms budgeted for all of that flakes 100% of the time
	// under parallel load/contention because round-trip latency alone can
	// exceed it; a few seconds gives real round trips headroom while still
	// keeping the test fast.
	const tinyTTL = 3 * time.Second
	svc := ledger.NewTransactionService(repo, nil, nil, ledger.WithIdempotencyTTL(tinyTTL))
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "idempotency ttl test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("create debit: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, credit); err != nil {
		t.Fatalf("create credit: %v", err)
	}

	idem := &domain.Idempotency{Key: "ttl-key"}
	first := mkTxn(t, debit.ID, credit.ID)
	if _, err := svc.Post(ctx, tenant, first, idem); err != nil {
		t.Fatalf("first post: %v", err)
	}

	// Immediately retrying with the SAME body, before the ttl elapses,
	// still replays.
	retryBeforeExpiry := mkTxn(t, debit.ID, credit.ID)
	replayed, err := svc.Post(ctx, tenant, retryBeforeExpiry, idem)
	if err != nil {
		t.Fatalf("retry before expiry: %v", err)
	}
	if !replayed || retryBeforeExpiry.ID != first.ID {
		t.Fatalf("retry before expiry: replayed=%v id=%s, want replayed=true id=%s", replayed, retryBeforeExpiry.ID, first.ID)
	}

	// Sleep past the ttl with its own margin on top (not a multiple of
	// tinyTTL, which would make this needlessly slow now that tinyTTL is
	// seconds rather than milliseconds): the point is only that this elapses
	// after expires_at, proving the expiry path, not how tiny the ttl was.
	time.Sleep(tinyTTL + 2*time.Second)

	// Past the ttl, the SAME key with a DIFFERENT body (a different amount)
	// is no longer a conflict: the expired key reads back as absent, so this
	// posts as a brand-new transaction instead of returning
	// ErrIdempotencyConflict.
	d, _ := domain.NewMoney(999, "USD")
	c, _ := domain.NewMoney(-999, "USD")
	afterExpiry := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit.ID, Amount: d},
		{AccountID: credit.ID, Amount: c},
	}}
	replayed, err = svc.Post(ctx, tenant, afterExpiry, idem)
	if err != nil {
		t.Fatalf("post after expiry: got %v, want nil (expired key must not conflict)", err)
	}
	if replayed {
		t.Error("post after expiry: replayed = true, want false (a new transaction, not a replay)")
	}
	if afterExpiry.ID == first.ID {
		t.Error("post after expiry produced the SAME transaction id as the original: expected a new transaction")
	}

	// The original transaction is untouched: expiry replaces the KEY's
	// binding, never rewrites history.
	original, err := repo.GetTransaction(ctx, tenant, first.ID)
	if err != nil {
		t.Fatalf("get original transaction: %v", err)
	}
	if len(original.Postings) != 2 || original.Postings[0].Amount.Amount() != 250 {
		t.Errorf("original transaction changed: %+v", original)
	}
}
