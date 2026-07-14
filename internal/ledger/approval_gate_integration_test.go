package ledger_test

// Task 6 (ADR-025): wiring ApprovalConfig.Gate (Task 5) and pending_transactions
// storage (Task 4) into Post. An under-threshold post behaves exactly as
// before; an over-threshold post is held as a pending instead of written to
// transactions, and returns a *ledger.HeldForApprovalError the caller can
// unwrap via ledger.AsHeldForApproval. The hold itself writes one
// approval.requested outbox row, subject to the pending, not to any
// transaction (there is no transaction: nothing was posted).

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newApprovalGateAccounts creates a tenant and a debit/credit USD account
// pair for the approval gate tests, mirroring newReverseAccounts' shape.
func newApprovalGateAccounts(t *testing.T, repo *postgres.Repository, tenant string) (debit, credit domain.Account) {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "approval gate test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	d := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	c := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, d); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, c); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	return *d, *c
}

// gatedTxn builds a balanced two-leg USD transaction of the given absolute
// amount (minor units), the same debit/credit shape mkTxn uses but with a
// caller-chosen size so a test can cross an approval threshold deliberately.
func gatedTxn(debit, credit string, amount int64) *domain.Transaction {
	d, _ := domain.NewMoney(amount, "USD")
	c, _ := domain.NewMoney(-amount, "USD")
	return &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: d, Description: "approval gate test"},
		{AccountID: credit, Amount: c, Description: "approval gate test"},
	}}
}

func TestApprovalGate_UnderThresholdPostsNormally(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	txn := gatedTxn(debit.ID, credit.ID, 50000)
	if _, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "under-threshold-1"}); err != nil {
		t.Fatalf("Post() under threshold error = %v, want nil", err)
	}

	got, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("GetTransaction() after under-threshold post: %v", err)
	}
	if got.ID != txn.ID {
		t.Fatalf("GetTransaction().ID = %q, want %q", got.ID, txn.ID)
	}

	pendings, err := repo.ListPendingTransactions(ctx, tenant, nil, nil, 10)
	if err != nil {
		t.Fatalf("ListPendingTransactions: %v", err)
	}
	if len(pendings) != 0 {
		t.Fatalf("ListPendingTransactions() = %d rows, want 0 for an under-threshold post", len(pendings))
	}
}

func TestApprovalGate_OverThresholdHeldAsPending(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	txn := gatedTxn(debit.ID, credit.ID, 150000)
	replayed, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: "over-threshold-1"})
	if err == nil {
		t.Fatal("Post() over threshold error = nil, want a *ledger.HeldForApprovalError")
	}
	if replayed {
		t.Error("Post() over threshold replayed = true, want false")
	}
	pending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}
	if pending.Status != domain.PendingStatusPending {
		t.Errorf("pending.Status = %q, want %q", pending.Status, domain.PendingStatusPending)
	}
	if pending.Kind != domain.PendingKindPost {
		t.Errorf("pending.Kind = %q, want %q", pending.Kind, domain.PendingKindPost)
	}
	if pending.ThresholdCcy != "USD" {
		t.Errorf("pending.ThresholdCcy = %q, want USD", pending.ThresholdCcy)
	}

	// The pending row is durable: a fresh read (not just the in-memory value
	// this call handed back) confirms it, matching the brief's "a
	// pending_transactions row exists" requirement.
	stored, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if stored.Status != domain.PendingStatusPending {
		t.Errorf("stored pending.Status = %q, want pending", stored.Status)
	}

	// transactions is unchanged: nothing was ever posted, so txn.ID (still
	// its client-side zero/whatever CreateTransaction would have assigned)
	// names no row. Post never assigned txn.ID because CreateTransaction was
	// never reached, so this checks the general absence of ANY new
	// transaction, via ListTransactions rather than a specific id.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListTransactions() = %d rows, want 0: an over-threshold post must not write to transactions", len(items))
	}

	// An approval.requested audit event was written to the outbox, subject
	// to the pending (Task 3's v2 subject events).
	tid, err := uuid.Parse(tenant)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}
	pid, err := uuid.Parse(pending.ID)
	if err != nil {
		t.Fatalf("parse pending id: %v", err)
	}
	var count int
	row := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_outbox WHERE tenant_id = $1 AND action = 'approval.requested' AND subject_id = $2 AND subject_type = 'pending_transaction'`,
		tid, pid)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query audit_outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("approval.requested outbox rows for pending %s = %d, want 1", pending.ID, count)
	}
}

// countPendingsForKey counts pending_transactions rows for tenant carrying
// exactly idempotencyKey, the row-count assertion
// TestApprovalGate_ReplaySameIdempotencyKeyReturnsSamePending needs beyond
// what the service API alone can confirm: two service calls both reporting
// the same pending id is consistent with either one row or two identically
// re-derived ids, so the test also checks the table directly.
func countPendingsForKey(t *testing.T, pool *pgxpool.Pool, tenant, idempotencyKey string) int {
	t.Helper()
	tid, err := uuid.Parse(tenant)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}
	var count int
	row := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pending_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
		tid, idempotencyKey)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query pending_transactions: %v", err)
	}
	return count
}

// TestApprovalGate_ReplaySameIdempotencyKeyReturnsSamePending is the ADR-025
// section 6 regression: a gated create must consume its idempotency key
// against the pending it holds, so a replay of the same over-threshold
// request with the same Idempotency-Key returns the SAME pending rather than
// holding a second one. Before the fix, holdForApproval never looked at (or
// stored) the caller's idempotency key at all, so a dropped-202-then-retried
// client would double the pending, and approving both would double-post the
// same money under two different derived approval keys.
func TestApprovalGate_ReplaySameIdempotencyKeyReturnsSamePending(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	const key = "gated-replay-1"

	txn1 := gatedTxn(debit.ID, credit.ID, 150000)
	_, err := svc.Post(ctx, tenant, txn1, &domain.Idempotency{Key: key})
	if err == nil {
		t.Fatal("Post() first over-threshold call error = nil, want a *ledger.HeldForApprovalError")
	}
	firstPending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}

	// Same payload, same idempotency key: a replay of a request whose 202
	// response the client never saw (or retried after a timeout).
	txn2 := gatedTxn(debit.ID, credit.ID, 150000)
	_, err = svc.Post(ctx, tenant, txn2, &domain.Idempotency{Key: key})
	if err == nil {
		t.Fatal("Post() replayed over-threshold call error = nil, want a *ledger.HeldForApprovalError")
	}
	secondPending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}

	if secondPending.ID != firstPending.ID {
		t.Fatalf("replayed pending.ID = %q, want the same pending %q", secondPending.ID, firstPending.ID)
	}

	if got := countPendingsForKey(t, pool, tenant, key); got != 1 {
		t.Fatalf("pending_transactions rows for tenant+key = %d, want exactly 1 (no duplicate pending)", got)
	}
}

// TestApprovalGate_NoIdempotencyKeyStillHoldsFreshPendingEachTime confirms
// the fix's boundary: a held create with NO Idempotency-Key at all has
// nothing to dedup against (NULL never collides under the partial unique
// index), so every call still creates its own pending, exactly like before
// this fix.
func TestApprovalGate_NoIdempotencyKeyStillHoldsFreshPendingEachTime(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	txn1 := gatedTxn(debit.ID, credit.ID, 150000)
	_, err := svc.Post(ctx, tenant, txn1, nil)
	if err == nil {
		t.Fatal("Post() first no-key over-threshold call error = nil, want a *ledger.HeldForApprovalError")
	}
	firstPending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}

	txn2 := gatedTxn(debit.ID, credit.ID, 150000)
	_, err = svc.Post(ctx, tenant, txn2, nil)
	if err == nil {
		t.Fatal("Post() second no-key over-threshold call error = nil, want a *ledger.HeldForApprovalError")
	}
	secondPending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}

	if secondPending.ID == firstPending.ID {
		t.Fatalf("second no-key pending.ID = %q, want a distinct pending from the first %q", secondPending.ID, firstPending.ID)
	}

	pendings, err := repo.ListPendingTransactions(ctx, tenant, nil, nil, 10)
	if err != nil {
		t.Fatalf("ListPendingTransactions: %v", err)
	}
	if len(pendings) != 2 {
		t.Fatalf("ListPendingTransactions() = %d rows, want 2 (a fresh pending per no-key hold)", len(pendings))
	}
}

// TestApprovalGate_DisablingGateDoesNotDoublePostHeldKey is the audit-remediation
// regression for the config-change double-post window: a request held as a
// pending under an idempotency key while the gate was ON must not post a SECOND
// transaction when the client retries the same key after the gate is turned OFF.
// dedupPendingForKey catches the still-pending hold and returns it as held
// instead of posting fresh.
func TestApprovalGate_DisablingGateDoesNotDoublePostHeldKey(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)
	const key = "gate-toggle-1"

	// Gate ON: an over-threshold post is held as a pending under the key.
	gated := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{Enabled: true, Thresholds: map[string]int64{"USD": 100000}}),
	)
	_, err := gated.Post(ctx, tenant, gatedTxn(debit.ID, credit.ID, 150000), &domain.Idempotency{Key: key})
	firstPending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("first over-threshold post: AsHeldForApproval(%v) ok = false, want held", err)
	}

	// Gate OFF (config changed), same key retried: must NOT post a second
	// transaction; the still-pending hold is returned instead.
	ungated := ledger.NewTransactionService(repo, discardLogger(), nil)
	_, err = ungated.Post(ctx, tenant, gatedTxn(debit.ID, credit.ID, 150000), &domain.Idempotency{Key: key})
	retryPending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("retry after disabling the gate: AsHeldForApproval(%v) ok = false, want the existing pending", err)
	}
	if retryPending.ID != firstPending.ID {
		t.Errorf("retry pending.ID = %q, want the original %q", retryPending.ID, firstPending.ID)
	}

	// No transaction was ever posted for this held request.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("ListTransactions() = %d rows, want 0 (the held request must not double-post when the gate is disabled)", len(items))
	}
}
