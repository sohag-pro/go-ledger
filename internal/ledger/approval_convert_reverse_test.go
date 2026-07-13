package ledger_test

// Coverage follow-up for Task 6/7 (ADR-025): the existing approval gate and
// approval service tests only ever exercise an over-threshold PLAIN POST.
// Convert and ReverseTransaction wire the exact same gate (Convert's own
// doc comment in convert.go, reverse.go's in ReverseTransaction), but their
// own hold-then-approve paths, and the reversal exemption for an
// already-approved original, were never driven end to end. These tests
// close that gap: an over-threshold Convert holds and later approves
// (covers convertPayload, Convert's gate branch, and replay's convert
// case), an over-threshold ReverseTransaction holds and later approves
// (covers reversePayload, ReverseTransaction's gate branch, and replay's
// reverse case), and a reversal of an already-approved original is never
// held at all (covers the PendingApprovedForTransaction exemption).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestApprovalGate_OverThresholdConvertHeldThenApproved builds a convert
// request whose USD leg exceeds the configured threshold, checks it is held
// as a kind-convert pending rather than posted, then approves it and checks
// the conversion actually lands: four postings, an FX snapshot, and the
// pending linked to the resulting transaction.
func TestApprovalGate_OverThresholdConvertHeldThenApproved(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	cfg := ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 5_000},
	}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithFXProvider(fx.NewDBProvider(pool)),
		ledger.WithApproval(cfg),
	)
	approvals := ledger.NewApprovalService(repo, svc, cfg, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()

	seedConvertRate(t, pool, "EUR", 92_000_000, 50)
	usd := newConvertAccount(t, repo, tenant, "USD")
	eur := newConvertAccount(t, repo, tenant, "EUR")

	const sourceAmount = 10_000 // over the 5,000 USD threshold
	req := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: sourceAmount}
	tx, replayed, err := svc.Convert(ctx, tenant, req, &domain.Idempotency{Key: "convert-gate-1"})
	if err == nil {
		t.Fatal("Convert() over threshold error = nil, want a *ledger.HeldForApprovalError")
	}
	if tx != nil {
		t.Errorf("Convert() over threshold tx = %+v, want nil", tx)
	}
	if replayed {
		t.Error("Convert() over threshold replayed = true, want false")
	}
	pending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}
	if pending.Kind != domain.PendingKindConvert {
		t.Errorf("pending.Kind = %q, want %q", pending.Kind, domain.PendingKindConvert)
	}
	if pending.ThresholdCcy != "USD" {
		t.Errorf("pending.ThresholdCcy = %q, want USD", pending.ThresholdCcy)
	}

	stored, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if stored.Status != domain.PendingStatusPending {
		t.Errorf("stored pending.Status = %q, want pending", stored.Status)
	}

	// Nothing was posted: the four convert legs never landed.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListTransactions() = %d rows, want 0: an over-threshold convert must not post", len(items))
	}

	// Approve it: replay dispatches the convert branch, resolving a fresh
	// rate against current state (the same rate here, since nothing moved).
	posted, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() error = %v, want nil", err)
	}
	if len(posted.Postings) != 4 {
		t.Fatalf("Approve() posted tx postings = %d, want 4", len(posted.Postings))
	}
	if posted.FX == nil {
		t.Fatal("Approve() posted tx has no FX detail, want a snapshot from the replayed convert")
	}
	if posted.FX.SourceAmount != sourceAmount {
		t.Errorf("posted tx FX.SourceAmount = %d, want %d", posted.FX.SourceAmount, sourceAmount)
	}

	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q) after approve: %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusApproved {
		t.Errorf("pending.Status after approve = %q, want approved", storedPending.Status)
	}
	if storedPending.TransactionID == nil || *storedPending.TransactionID != posted.ID {
		t.Errorf("pending.TransactionID = %v, want %q", storedPending.TransactionID, posted.ID)
	}

	// The conversion really moved money: the USD account is down by the
	// source amount.
	usdBal, err := repo.Balance(ctx, tenant, usd.ID)
	if err != nil {
		t.Fatalf("balance usd: %v", err)
	}
	if usdBal.Amount() != -sourceAmount {
		t.Errorf("usd balance after approved convert = %d, want %d", usdBal.Amount(), -sourceAmount)
	}
}

// TestApprovalGate_OverThresholdReverseHeldThenApproved posts a large
// original transaction through a plain (ungated) service, then reverses it
// through a gated service whose threshold the reversal's legs exceed. The
// reversal must be held as a kind-reverse pending, not posted, and approving
// it must replay ReverseTransaction and actually restore both accounts to
// zero.
func TestApprovalGate_OverThresholdReverseHeldThenApproved(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	// Post the large original through an ungated service: only the
	// REVERSAL's gate behavior is under test here.
	plainSvc := ledger.NewTransactionService(repo, discardLogger(), nil)
	original := gatedTxn(debit.ID, credit.ID, holdGatedAmount)
	if _, err := plainSvc.Post(ctx, tenant, original, &domain.Idempotency{Key: "reverse-gate-original-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	cfg := ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100_000},
	}
	gatedSvc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithApproval(cfg))
	approvals := ledger.NewApprovalService(repo, gatedSvc, cfg, discardLogger())

	_, alreadyReversed, err := gatedSvc.ReverseTransaction(ctx, tenant, original.ID)
	if err == nil {
		t.Fatal("ReverseTransaction() over threshold error = nil, want a *ledger.HeldForApprovalError")
	}
	if alreadyReversed {
		t.Error("ReverseTransaction() over threshold alreadyReversed = true, want false")
	}
	pending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}
	if pending.Kind != domain.PendingKindReverse {
		t.Errorf("pending.Kind = %q, want %q", pending.Kind, domain.PendingKindReverse)
	}

	// Nothing reversed yet.
	if _, err := repo.GetReversalOf(ctx, tenant, original.ID); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("GetReversalOf before approval: err = %v, want ErrTransactionNotFound", err)
	}
	debitBalBeforeApprove, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance before approve: %v", err)
	}
	if debitBalBeforeApprove.Amount() != holdGatedAmount {
		t.Fatalf("debit balance before approve = %d, want %d (the reversal has not posted)", debitBalBeforeApprove.Amount(), holdGatedAmount)
	}

	tx, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() error = %v, want nil", err)
	}
	if tx.ReversesTransactionID == nil || *tx.ReversesTransactionID != original.ID {
		t.Errorf("approved reversal ReversesTransactionID = %v, want pointer to %q", tx.ReversesTransactionID, original.ID)
	}

	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q) after approve: %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusApproved {
		t.Errorf("pending.Status after approve = %q, want approved", storedPending.Status)
	}
	if storedPending.TransactionID == nil || *storedPending.TransactionID != tx.ID {
		t.Errorf("pending.TransactionID = %v, want %q", storedPending.TransactionID, tx.ID)
	}

	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance after approve: %v", err)
	}
	if debitBal.Amount() != 0 {
		t.Errorf("debit balance after approved reversal = %d, want 0", debitBal.Amount())
	}
}

// TestApprovalGate_ReverseOfApprovedPendingNotHeldAgain covers ADR-025's
// reversal exemption: reversing a transaction that itself came from an
// approved pending must NOT be held again, even though the reversal's own
// legs are just as far over the threshold as the original post was.
func TestApprovalGate_ReverseOfApprovedPendingNotHeldAgain(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	cfg := ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100_000},
	}
	svc := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithApproval(cfg))
	approvals := ledger.NewApprovalService(repo, svc, cfg, discardLogger())

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)
	tx, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() original post error = %v, want nil", err)
	}

	// Reversing tx is just as far over the threshold as the original post
	// was, but tx's own pending is already approved, so this must post
	// directly rather than hold a second pending.
	reversal, alreadyReversed, err := svc.ReverseTransaction(ctx, tenant, tx.ID)
	if err != nil {
		t.Fatalf("ReverseTransaction() of an approved-pending original error = %v, want nil (exemption)", err)
	}
	if alreadyReversed {
		t.Error("ReverseTransaction() alreadyReversed = true, want false")
	}
	if reversal.ReversesTransactionID == nil || *reversal.ReversesTransactionID != tx.ID {
		t.Errorf("reversal.ReversesTransactionID = %v, want pointer to %q", reversal.ReversesTransactionID, tx.ID)
	}

	// No second (reverse-kind) pending was ever created.
	pendings, err := repo.ListPendingTransactions(ctx, tenant, nil, nil, 10)
	if err != nil {
		t.Fatalf("ListPendingTransactions: %v", err)
	}
	reverseCount := 0
	for _, p := range pendings {
		if p.Kind == domain.PendingKindReverse {
			reverseCount++
		}
	}
	if reverseCount != 0 {
		t.Errorf("pending_transactions rows of kind reverse = %d, want 0 (exempted)", reverseCount)
	}

	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance after exempted reversal: %v", err)
	}
	if debitBal.Amount() != 0 {
		t.Errorf("debit balance after exempted reversal = %d, want 0", debitBal.Amount())
	}
}
