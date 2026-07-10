package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newDisputeAccounts mirrors newReverseAccounts (reverse_test.go): a tenant
// plus a debit/credit account pair for dispute tests.
func newDisputeAccounts(t *testing.T, repo *postgres.Repository, tenant string) (debit, credit domain.Account) {
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "dispute test tenant"); err != nil {
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

// TestDisputeService_OpenAndResolveReverse is the key proof (Task 6.3, audit
// A9.2): resolving with "reverse" posts a REAL reversal through
// TransactionService.ReverseTransaction, not a raw insert, restores
// balances, links resolution_transaction_id to the reversal, and moves
// status to resolved_reversed.
func TestDisputeService_OpenAndResolveReverse(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := txns.Post(ctx, tenant, original, &domain.Idempotency{Key: "dispute-resolve-reverse-post-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	d, err := disputes.Open(ctx, tenant, original.ID, "customer chargeback")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Status != domain.DisputeOpen {
		t.Errorf("status = %q, want %q", d.Status, domain.DisputeOpen)
	}

	resolved, err := disputes.Resolve(ctx, tenant, d.ID, ledger.DisputeActionReverse)
	if err != nil {
		t.Fatalf("Resolve(reverse): %v", err)
	}
	if resolved.Status != domain.DisputeResolvedReversed {
		t.Errorf("status = %q, want %q", resolved.Status, domain.DisputeResolvedReversed)
	}
	if resolved.ResolutionTransactionID == nil {
		t.Fatal("resolution_transaction_id = nil, want the reversal's id")
	}
	if resolved.ResolvedAt == nil {
		t.Error("resolved_at = nil, want a timestamp")
	}

	// The linked id is a REAL transaction that reverses the original: this
	// is the proof that Resolve went through ReverseTransaction (the normal
	// posting path), not a raw insert. GetReversalOf is the same repository
	// read TransactionService.ReverseTransaction itself uses as its
	// idempotency precheck, so this also proves there is exactly one
	// reversal on file.
	reversal, err := repo.GetTransaction(ctx, tenant, *resolved.ResolutionTransactionID)
	if err != nil {
		t.Fatalf("get reversal: %v", err)
	}
	if reversal.ReversesTransactionID == nil || *reversal.ReversesTransactionID != original.ID {
		t.Errorf("reversal.ReversesTransactionID = %v, want pointer to %q", reversal.ReversesTransactionID, original.ID)
	}
	viaGetReversalOf, err := repo.GetReversalOf(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("GetReversalOf: %v", err)
	}
	if viaGetReversalOf.ID != reversal.ID {
		t.Errorf("GetReversalOf id = %q, want %q (the same reversal Resolve linked)", viaGetReversalOf.ID, reversal.ID)
	}

	// Balances are restored: the reversal's negated legs cancel the
	// original's, exactly like a direct POST /v1/transactions/{id}/reverse
	// would (TestReverseTransaction_RestoresBalances).
	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance: %v", err)
	}
	if debitBal.Amount() != 0 {
		t.Errorf("debit balance after dispute-driven reversal = %d, want 0", debitBal.Amount())
	}
}

// TestDisputeService_ResolveReject checks that "reject" moves no money and
// lands resolved_rejected with no resolution transaction.
func TestDisputeService_ResolveReject(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := txns.Post(ctx, tenant, original, &domain.Idempotency{Key: "dispute-resolve-reject-post-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	d, err := disputes.Open(ctx, tenant, original.ID, "chargeback")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	resolved, err := disputes.Resolve(ctx, tenant, d.ID, ledger.DisputeActionReject)
	if err != nil {
		t.Fatalf("Resolve(reject): %v", err)
	}
	if resolved.Status != domain.DisputeResolvedRejected {
		t.Errorf("status = %q, want %q", resolved.Status, domain.DisputeResolvedRejected)
	}
	if resolved.ResolutionTransactionID != nil {
		t.Errorf("resolution_transaction_id = %v, want nil (no money moved)", resolved.ResolutionTransactionID)
	}

	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance: %v", err)
	}
	if debitBal.Amount() != 250 {
		t.Errorf("debit balance after reject = %d, want 250 (unchanged, mkTxn's fixed amount)", debitBal.Amount())
	}

	// No reversal exists: GetReversalOf must report not-found.
	if _, err := repo.GetReversalOf(ctx, tenant, original.ID); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("GetReversalOf after reject: err = %v, want ErrTransactionNotFound (no reversal posted)", err)
	}
}

// TestDisputeService_OpenUnknownTransaction checks that opening a dispute
// against a transaction id that names nothing (in this tenant) fails with
// ErrTransactionNotFound, including when the id belongs to ANOTHER tenant
// (cross-tenant isolation): the composite FK and the up-front GetTransaction
// check both back this, but this test only exercises the service path.
func TestDisputeService_OpenUnknownTransaction(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "dispute unknown txn tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	if _, err := disputes.Open(ctx, tenant, uuid.NewString(), "reason"); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("Open on unknown id: err = %v, want ErrTransactionNotFound", err)
	}

	// Cross-tenant: a transaction that exists, but in a DIFFERENT tenant.
	otherTenant := uuid.NewString()
	otherDebit, otherCredit := newDisputeAccounts(t, repo, otherTenant)
	otherTxn := mkTxn(t, otherDebit.ID, otherCredit.ID)
	if _, err := txns.Post(ctx, otherTenant, otherTxn, &domain.Idempotency{Key: "dispute-cross-tenant-1"}); err != nil {
		t.Fatalf("post other-tenant txn: %v", err)
	}
	if _, err := disputes.Open(ctx, tenant, otherTxn.ID, "reason"); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("Open on another tenant's transaction: err = %v, want ErrTransactionNotFound", err)
	}
}

// TestDisputeService_OpenInvalidReason proves Open validates the dispute
// before ever writing it: an empty reason (or one over
// domain.MaxDisputeReasonLen) is rejected with a domain validation error,
// and, since CreateDispute is never reached, no dispute row is left behind
// for the same transaction (a later Open with a valid reason still
// succeeds).
func TestDisputeService_OpenInvalidReason(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := txns.Post(ctx, tenant, original, &domain.Idempotency{Key: "dispute-open-invalid-reason-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}

	if _, err := disputes.Open(ctx, tenant, original.ID, ""); !errors.Is(err, domain.ErrInvalidDispute) {
		t.Errorf("Open with empty reason: err = %v, want ErrInvalidDispute", err)
	}

	tooLong := make([]byte, domain.MaxDisputeReasonLen+1)
	for i := range tooLong {
		tooLong[i] = 'x'
	}
	if _, err := disputes.Open(ctx, tenant, original.ID, string(tooLong)); !errors.Is(err, domain.ErrDisputeReasonTooLong) {
		t.Errorf("Open with too-long reason: err = %v, want ErrDisputeReasonTooLong", err)
	}

	// No dispute was ever created for this transaction: a valid Open still
	// succeeds afterward.
	d, err := disputes.Open(ctx, tenant, original.ID, "a valid reason")
	if err != nil {
		t.Fatalf("Open with a valid reason after two rejected attempts: %v", err)
	}
	if d.Status != domain.DisputeOpen {
		t.Errorf("status = %q, want %q", d.Status, domain.DisputeOpen)
	}
}

// TestDisputeService_ResolveTwiceIsRejected checks that resolving an
// already-resolved dispute (sequentially, not racing) returns
// ErrDisputeAlreadyResolved and never posts a second reversal.
func TestDisputeService_ResolveTwiceIsRejected(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := txns.Post(ctx, tenant, original, &domain.Idempotency{Key: "dispute-resolve-twice-post-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	d, err := disputes.Open(ctx, tenant, original.ID, "chargeback")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	first, err := disputes.Resolve(ctx, tenant, d.ID, ledger.DisputeActionReverse)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	if _, err := disputes.Resolve(ctx, tenant, d.ID, ledger.DisputeActionReject); !errors.Is(err, domain.ErrDisputeAlreadyResolved) {
		t.Errorf("second Resolve: err = %v, want ErrDisputeAlreadyResolved", err)
	}

	// Still exactly one reversal on file: the second (rejected) resolve
	// attempt must not have posted anything.
	viaGetReversalOf, err := repo.GetReversalOf(ctx, tenant, original.ID)
	if err != nil {
		t.Fatalf("GetReversalOf: %v", err)
	}
	if viaGetReversalOf.ID != *first.ResolutionTransactionID {
		t.Errorf("reversal on file = %q, want the first resolve's own reversal %q", viaGetReversalOf.ID, *first.ResolutionTransactionID)
	}
}

// TestDisputeService_ResolveInvalidAction checks that an action other than
// "reverse" or "reject" is rejected without touching the dispute at all.
func TestDisputeService_ResolveInvalidAction(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := txns.Post(ctx, tenant, original, &domain.Idempotency{Key: "dispute-invalid-action-post-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	d, err := disputes.Open(ctx, tenant, original.ID, "chargeback")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := disputes.Resolve(ctx, tenant, d.ID, "bogus"); !errors.Is(err, domain.ErrInvalidDisputeAction) {
		t.Errorf("Resolve(bogus): err = %v, want ErrInvalidDisputeAction", err)
	}

	// The dispute must still be open: an invalid action must not have
	// changed anything.
	stillOpen, err := disputes.Get(ctx, tenant, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillOpen.Status != domain.DisputeOpen {
		t.Errorf("status after invalid action = %q, want still %q", stillOpen.Status, domain.DisputeOpen)
	}
}

// TestDisputeService_ScreeningRejectsAReversal proves the resolve->reversal
// linkage goes through the REAL posting path, not a raw insert (Task 6.3,
// audit A9.2): a screening hook that vetoes reversals must be able to block
// a dispute-driven reversal too. This is the "the reversal went through the
// normal posting path" proof the brief calls for.
func TestDisputeService_ScreeningRejectsAReversal(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	hook := &stubPrePostHook{}
	txns := ledger.NewTransactionService(repo, discardLogger(), nil, ledger.WithPrePostHook(hook))
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	original := mkTxn(t, debit.ID, credit.ID)
	if _, err := txns.Post(ctx, tenant, original, &domain.Idempotency{Key: "dispute-screening-post-1"}); err != nil {
		t.Fatalf("post original: %v", err)
	}
	d, err := disputes.Open(ctx, tenant, original.ID, "chargeback")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	hook.err = &ledger.ScreeningRejectedError{Reason: "sanctions list match"}
	if _, err := disputes.Resolve(ctx, tenant, d.ID, ledger.DisputeActionReverse); !errors.Is(err, ledger.ErrScreeningRejected) {
		t.Fatalf("Resolve(reverse) with a rejecting hook: err = %v, want errors.Is match on ledger.ErrScreeningRejected", err)
	}

	// The dispute must still be open: a screening-rejected reversal must
	// not have flipped it to resolved_reversed with nothing to back it.
	stillOpen, err := disputes.Get(ctx, tenant, d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stillOpen.Status != domain.DisputeOpen {
		t.Errorf("status after screening-rejected reverse = %q, want still %q", stillOpen.Status, domain.DisputeOpen)
	}
	if _, err := repo.GetReversalOf(ctx, tenant, original.ID); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("GetReversalOf after screening-rejected resolve: err = %v, want ErrTransactionNotFound (nothing posted)", err)
	}

	// Balances are untouched: no reversal, no money movement.
	debitBal, err := repo.Balance(ctx, tenant, debit.ID)
	if err != nil {
		t.Fatalf("debit balance: %v", err)
	}
	if debitBal.Amount() != 250 {
		t.Errorf("debit balance after screening-rejected resolve = %d, want 250 (unchanged)", debitBal.Amount())
	}
}

// TestDisputeService_List checks status filtering and keyset paging over a
// real Postgres, mirroring TestListTransactions-style coverage in this
// package.
func TestDisputeService_List(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	disputes := ledger.NewDisputeService(repo, txns)
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newDisputeAccounts(t, repo, tenant)

	var opened []domain.Dispute
	for i := 0; i < 3; i++ {
		txn := mkTxn(t, debit.ID, credit.ID)
		if _, err := txns.Post(ctx, tenant, txn, &domain.Idempotency{Key: uuid.NewString()}); err != nil {
			t.Fatalf("post txn %d: %v", i, err)
		}
		d, err := disputes.Open(ctx, tenant, txn.ID, "reason")
		if err != nil {
			t.Fatalf("open dispute %d: %v", i, err)
		}
		opened = append(opened, d)
	}
	if _, err := disputes.Resolve(ctx, tenant, opened[0].ID, ledger.DisputeActionReject); err != nil {
		t.Fatalf("resolve dispute 0: %v", err)
	}

	all, err := disputes.List(ctx, tenant, nil, nil, 50)
	if err != nil {
		t.Fatalf("List (no filter): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List (no filter) = %d, want 3", len(all))
	}

	openStatus := domain.DisputeOpen
	openOnly, err := disputes.List(ctx, tenant, &openStatus, nil, 50)
	if err != nil {
		t.Fatalf("List (open): %v", err)
	}
	if len(openOnly) != 2 {
		t.Fatalf("List (open) = %d, want 2", len(openOnly))
	}
	for _, d := range openOnly {
		if d.Status != domain.DisputeOpen {
			t.Errorf("List (open) returned status %q", d.Status)
		}
	}
}
