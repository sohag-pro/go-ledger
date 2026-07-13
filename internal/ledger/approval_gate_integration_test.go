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
