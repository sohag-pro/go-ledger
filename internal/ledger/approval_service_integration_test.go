package ledger_test

// Task 7 (ADR-025): the ApprovalService that decides a pending transaction
// Task 6 held. Approve replays the pending's stored payload through the
// normal post path (Post/Convert/ReverseTransaction) against CURRENT
// balances, links the resulting transaction, and emits approval.approved;
// Reject and Cancel close a pending out without ever posting money. Every
// transition locks the pending row and updates status plus a lifecycle
// audit event atomically (see approval_service.go's lockAndCheck).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// holdGatedAmount is the leg size every holdGatedPost call in this file
// uses: comfortably over every test's configured USD threshold (100000).
const holdGatedAmount = 150000

// holdGatedPost posts a transaction guaranteed to trip svc's approval gate
// (holdGatedAmount, over every configured threshold) and returns the
// resulting pending, failing the test if the post was not actually held.
func holdGatedPost(t *testing.T, svc *ledger.TransactionService, tenant, debit, credit string) *domain.PendingTransaction {
	t.Helper()
	ctx := context.Background()
	txn := gatedTxn(debit, credit, holdGatedAmount)
	_, err := svc.Post(ctx, tenant, txn, &domain.Idempotency{Key: uuid.NewString()})
	if err == nil {
		t.Fatal("Post() over threshold error = nil, want a *ledger.HeldForApprovalError")
	}
	pending, ok := ledger.AsHeldForApproval(err)
	if !ok {
		t.Fatalf("AsHeldForApproval(%v) ok = false, want true", err)
	}
	return pending
}

func TestApprovalService_ApprovePostsLinksAndEmitsEvent(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	tx, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() error = %v, want nil", err)
	}
	if tx == nil || tx.ID == "" {
		t.Fatalf("Approve() tx = %+v, want a posted transaction with an id", tx)
	}

	// The transaction really landed in transactions.
	stored, err := repo.GetTransaction(ctx, tenant, tx.ID)
	if err != nil {
		t.Fatalf("GetTransaction(%q): %v", tx.ID, err)
	}
	if stored.ID != tx.ID {
		t.Fatalf("GetTransaction().ID = %q, want %q", stored.ID, tx.ID)
	}

	// The pending is now approved and linked to the transaction it produced.
	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusApproved {
		t.Errorf("pending.Status = %q, want approved", storedPending.Status)
	}
	if storedPending.TransactionID == nil || *storedPending.TransactionID != tx.ID {
		t.Errorf("pending.TransactionID = %v, want %q", storedPending.TransactionID, tx.ID)
	}

	// An approval.approved lifecycle event landed in the outbox, subject to
	// the pending.
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
		`SELECT count(*) FROM audit_outbox WHERE tenant_id = $1 AND action = 'approval.approved' AND subject_id = $2 AND subject_type = 'pending_transaction'`,
		tid, pid)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query audit_outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("approval.approved outbox rows for pending %s = %d, want 1", pending.ID, count)
	}

	// A second Approve is idempotent: same tx id, no second post.
	tx2, err := approvals.Approve(ctx, tenant, pending.ID, "approver-2")
	if err != nil {
		t.Fatalf("second Approve() error = %v, want nil", err)
	}
	if tx2.ID != tx.ID {
		t.Fatalf("second Approve().ID = %q, want the same tx id %q", tx2.ID, tx.ID)
	}
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ListTransactions() = %d rows, want exactly 1: a second Approve must not post again", len(items))
	}
}

func TestApprovalService_FourEyesBlocksSelfApproval(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:               true,
		Thresholds:            map[string]int64{"USD": 100000},
		RequireDifferentActor: true,
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	// This ledger-level test calls the service directly, with no HTTP/gRPC
	// layer to supply a per-key principal via ledger.WithActor, so actorOr
	// falls back to the tenant id: CreatedBy is the tenant, and approving as
	// the same tenant is the same-principal case four-eyes must block.
	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)
	if pending.CreatedBy != tenant {
		t.Fatalf("pending.CreatedBy = %q, want %q (tenant fallback principal)", pending.CreatedBy, tenant)
	}

	_, err := approvals.Approve(ctx, tenant, pending.ID, tenant)
	if !errors.Is(err, domain.ErrCannotApproveOwn) {
		t.Fatalf("Approve() by creator with RequireDifferentActor error = %v, want ErrCannotApproveOwn", err)
	}

	// Nothing was posted, and the pending is still pending.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListTransactions() = %d rows, want 0: a blocked self-approval must not post", len(items))
	}
	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusPending {
		t.Errorf("pending.Status = %q, want pending", storedPending.Status)
	}

	// A different actor can still approve it.
	tx, err := approvals.Approve(ctx, tenant, pending.ID, "a-different-approver")
	if err != nil {
		t.Fatalf("Approve() by different actor error = %v, want nil", err)
	}
	if tx == nil || tx.ID == "" {
		t.Fatalf("Approve() by different actor tx = %+v, want a posted transaction", tx)
	}
}

func TestApprovalService_RejectThenApproveConflicts(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	reason := "does not look right"
	if err := approvals.Reject(ctx, tenant, pending.ID, "reviewer-1", &reason); err != nil {
		t.Fatalf("Reject() error = %v, want nil", err)
	}

	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusRejected {
		t.Errorf("pending.Status = %q, want rejected", storedPending.Status)
	}
	if storedPending.Reason == nil || *storedPending.Reason != reason {
		t.Errorf("pending.Reason = %v, want %q", storedPending.Reason, reason)
	}

	// Nothing was ever posted.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListTransactions() = %d rows, want 0: a rejected pending must never post", len(items))
	}

	// The rejected.rejected lifecycle event landed in the outbox.
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
		`SELECT count(*) FROM audit_outbox WHERE tenant_id = $1 AND action = 'approval.rejected' AND subject_id = $2 AND subject_type = 'pending_transaction'`,
		tid, pid)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query audit_outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("approval.rejected outbox rows for pending %s = %d, want 1", pending.ID, count)
	}

	// Approving a rejected pending is a 409-class conflict, not a post.
	_, err = approvals.Approve(ctx, tenant, pending.ID, "reviewer-2")
	if !errors.Is(err, domain.ErrPendingAlreadyDecided) {
		t.Fatalf("Approve() on rejected pending error = %v, want ErrPendingAlreadyDecided", err)
	}
}

// TestApprovalService_RejectCannotOrphanApprovedPending is a deterministic,
// sequential simulation of the ordering a concurrent Approve-vs-Reject race
// would produce: Approve wins and links a transaction first, then a Reject
// against the SAME pending arrives. It must be refused with
// ErrPendingAlreadyDecided rather than flipping the pending back to
// rejected and clearing its transaction_id, which would orphan the posted
// transaction (money already moved with nothing pending left to show for
// it). This exercises exactly the write-side guard the fix adds: Reject now
// locks the row and re-checks status atomically with its write
// (lockedTransition), so it can never race a concurrent decision's write.
func TestApprovalService_RejectCannotOrphanApprovedPending(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	tx, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() error = %v, want nil", err)
	}

	// A Reject that arrives after the pending is already approved must be
	// refused, not silently flip a linked, posted pending back to rejected.
	reason := "too late, already approved"
	err = approvals.Reject(ctx, tenant, pending.ID, "reviewer-1", &reason)
	if !errors.Is(err, domain.ErrPendingAlreadyDecided) {
		t.Fatalf("Reject() on an already-approved pending error = %v, want ErrPendingAlreadyDecided", err)
	}

	// The pending is still approved and still linked to the posted
	// transaction: the Reject must not have cleared transaction_id or
	// flipped the status.
	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusApproved {
		t.Errorf("pending.Status after refused Reject = %q, want approved", storedPending.Status)
	}
	if storedPending.TransactionID == nil || *storedPending.TransactionID != tx.ID {
		t.Errorf("pending.TransactionID after refused Reject = %v, want %q (must not be orphaned)", storedPending.TransactionID, tx.ID)
	}

	// The posted transaction really is still there, untouched.
	stored, err := repo.GetTransaction(ctx, tenant, tx.ID)
	if err != nil {
		t.Fatalf("GetTransaction(%q): %v", tx.ID, err)
	}
	if stored.ID != tx.ID {
		t.Fatalf("GetTransaction().ID = %q, want %q", stored.ID, tx.ID)
	}
}

// TestApprovalService_CancelCannotOrphanApprovedPending mirrors the Reject
// case above for Cancel: once a pending is approved and linked, a Cancel
// against it (even by its own creator) must be refused rather than orphan
// the posted transaction.
func TestApprovalService_CancelCannotOrphanApprovedPending(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	tx, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() error = %v, want nil", err)
	}

	// pending.CreatedBy is stamped as the tenant id (see the four-eyes test
	// above), so this Cancel call is made BY the creator: even the creator
	// cannot cancel out from under an already-approved, already-posted
	// pending.
	err = approvals.Cancel(ctx, tenant, pending.ID, pending.CreatedBy)
	if !errors.Is(err, domain.ErrPendingAlreadyDecided) {
		t.Fatalf("Cancel() on an already-approved pending error = %v, want ErrPendingAlreadyDecided", err)
	}

	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusApproved {
		t.Errorf("pending.Status after refused Cancel = %q, want approved", storedPending.Status)
	}
	if storedPending.TransactionID == nil || *storedPending.TransactionID != tx.ID {
		t.Errorf("pending.TransactionID after refused Cancel = %v, want %q (must not be orphaned)", storedPending.TransactionID, tx.ID)
	}
}

func TestApprovalService_CancelByCreatorOKByOthersRefused(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	// A non-creator cannot cancel.
	if err := approvals.Cancel(ctx, tenant, pending.ID, "not-the-creator"); !errors.Is(err, domain.ErrNotPendingCreator) {
		t.Fatalf("Cancel() by non-creator error = %v, want ErrNotPendingCreator", err)
	}
	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusPending {
		t.Errorf("pending.Status after refused cancel = %q, want pending", storedPending.Status)
	}

	// The creator can cancel (CreatedBy is stamped as the tenant id today,
	// see the four-eyes test above).
	if err := approvals.Cancel(ctx, tenant, pending.ID, pending.CreatedBy); err != nil {
		t.Fatalf("Cancel() by creator error = %v, want nil", err)
	}
	storedPending, err = repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusCancelled {
		t.Errorf("pending.Status = %q, want cancelled", storedPending.Status)
	}

	// Nothing was ever posted.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ListTransactions() = %d rows, want 0: a cancelled pending must never post", len(items))
	}
}

// TestApprovalService_ApproveRevalidatesAgainstCurrentBalances covers the
// re-validation guarantee: a pending is held over the threshold gate, but
// its own accounts are never balance-checked until it actually replays
// (Post's approval gate runs before the account-constraints check). Between
// the hold and the decision, a second, ordinary (under-threshold) post drives
// the debit account down near its configured floor (chosen so the held
// payload alone would still fit, but the drain plus the held payload
// together would not), so the held payload would now overdraw it. Approve
// must surface that validation error and leave the pending untouched, not
// post a broken transaction.
func TestApprovalService_ApproveRevalidatesAgainstCurrentBalances(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "revalidation test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	floor := int64(-180000)
	debit := &domain.Account{Name: "Checking", Type: domain.Asset, Currency: "USD", MinBalance: &floor}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, debit); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, credit); err != nil {
		t.Fatalf("create credit account: %v", err)
	}

	// gatedTxn(a, b, amount) posts +amount to a and -amount to b: to drive
	// the floored Checking account NEGATIVE, it must be passed as the
	// second (credited/debited-down) argument, Revenue as the first.
	//
	// Held over the gate: at hold time nothing checks the floor, only the
	// threshold.
	pending := holdGatedPost(t, svc, tenant, credit.ID, debit.ID)

	// A second, ordinary (under-threshold) post drains the debit account
	// down near its floor before the decision is made.
	drain := gatedTxn(credit.ID, debit.ID, 60000)
	if _, err := svc.Post(ctx, tenant, drain, &domain.Idempotency{Key: uuid.NewString()}); err != nil {
		t.Fatalf("Post() drain error = %v, want nil", err)
	}

	// Replaying the held 150000 debit now would take the account to
	// -60000-150000 = -210000, well below its -100000 floor.
	_, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	var breach *domain.MinBalanceBreachError
	if !errors.As(err, &breach) {
		t.Fatalf("Approve() error = %v, want *domain.MinBalanceBreachError", err)
	}

	// The pending stays pending: nothing about the failed replay committed.
	storedPending, err := repo.GetPendingTransaction(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(%q): %v", pending.ID, err)
	}
	if storedPending.Status != domain.PendingStatusPending {
		t.Errorf("pending.Status after failed re-validation = %q, want pending", storedPending.Status)
	}
	if storedPending.TransactionID != nil {
		t.Errorf("pending.TransactionID = %v, want nil: the failed replay must not link a transaction", storedPending.TransactionID)
	}

	// Only the drain transaction exists; the held 150000 never posted.
	items, err := repo.ListTransactions(ctx, tenant, domain.TransactionFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ListTransactions() = %d rows, want 1 (only the drain)", len(items))
	}

	// A retry after fixing the balance (reversing the drain) succeeds.
	if _, _, err := svc.ReverseTransaction(ctx, tenant, drain.ID); err != nil {
		t.Fatalf("ReverseTransaction(drain) error = %v, want nil", err)
	}
	tx, err := approvals.Approve(ctx, tenant, pending.ID, "approver-1")
	if err != nil {
		t.Fatalf("Approve() retry after fixing balance error = %v, want nil", err)
	}
	if tx == nil || tx.ID == "" {
		t.Fatalf("Approve() retry tx = %+v, want a posted transaction", tx)
	}
}

// TestApprovalService_GetAndList covers the read-side pass-throughs.
func TestApprovalService_GetAndList(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	approvals := ledger.NewApprovalService(repo, svc, ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
	}, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	pending := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	got, err := approvals.Get(ctx, tenant, pending.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", pending.ID, err)
	}
	if got.ID != pending.ID {
		t.Errorf("Get().ID = %q, want %q", got.ID, pending.ID)
	}

	pendingStatus := domain.PendingStatusPending
	list, err := approvals.List(ctx, tenant, &pendingStatus, nil, 10)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	found := false
	for _, p := range list {
		if p.ID == pending.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("List(pending) = %+v, want to contain %q", list, pending.ID)
	}
}

// TestApprovalService_SweepExpiredPendingExpiresOnlyStale covers the TTL
// sweep (Task 8, ADR-025): a pending left undecided past cfg.TTL is moved to
// expired with an approval.expired lifecycle event, while a fresh pending
// (well within the TTL) is left untouched.
func TestApprovalService_SweepExpiredPendingExpiresOnlyStale(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil,
		ledger.WithApproval(ledger.ApprovalConfig{
			Enabled:    true,
			Thresholds: map[string]int64{"USD": 100000},
		}),
	)
	cfg := ledger.ApprovalConfig{
		Enabled:    true,
		Thresholds: map[string]int64{"USD": 100000},
		TTL:        time.Hour,
	}
	approvals := ledger.NewApprovalService(repo, svc, cfg, discardLogger())
	ctx := context.Background()
	tenant := uuid.NewString()
	debit, credit := newApprovalGateAccounts(t, repo, tenant)

	stale := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)
	fresh := holdGatedPost(t, svc, tenant, debit.ID, credit.ID)

	// Backdate the stale pending's created_at well past the TTL. Only a raw
	// SQL statement can do this: InsertPendingTransaction leaves created_at
	// to its column default (now()), the same reason the demo seeder writes
	// created_at directly for its own backdated rows.
	tid, err := uuid.Parse(tenant)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}
	pid, err := uuid.Parse(stale.ID)
	if err != nil {
		t.Fatalf("parse stale pending id: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE pending_transactions SET created_at = now() - interval '2 hours' WHERE tenant_id = $1 AND id = $2`,
		tid, pid); err != nil {
		t.Fatalf("backdate stale pending: %v", err)
	}

	n, err := approvals.SweepExpiredPending(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredPending() error = %v, want nil", err)
	}
	if n != 1 {
		t.Fatalf("SweepExpiredPending() = %d, want 1", n)
	}

	storedStale, err := repo.GetPendingTransaction(ctx, tenant, stale.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(stale): %v", err)
	}
	if storedStale.Status != domain.PendingStatusExpired {
		t.Errorf("stale pending.Status = %q, want expired", storedStale.Status)
	}

	storedFresh, err := repo.GetPendingTransaction(ctx, tenant, fresh.ID)
	if err != nil {
		t.Fatalf("GetPendingTransaction(fresh): %v", err)
	}
	if storedFresh.Status != domain.PendingStatusPending {
		t.Errorf("fresh pending.Status = %q, want still pending", storedFresh.Status)
	}

	// An approval.expired lifecycle event landed in the outbox for the
	// stale pending, and only the stale one.
	var staleCount int
	row := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_outbox WHERE tenant_id = $1 AND action = 'approval.expired' AND subject_id = $2 AND subject_type = 'pending_transaction'`,
		tid, pid)
	if err := row.Scan(&staleCount); err != nil {
		t.Fatalf("query audit_outbox (stale): %v", err)
	}
	if staleCount != 1 {
		t.Fatalf("approval.expired outbox rows for stale pending %s = %d, want 1", stale.ID, staleCount)
	}

	freshPid, err := uuid.Parse(fresh.ID)
	if err != nil {
		t.Fatalf("parse fresh pending id: %v", err)
	}
	var freshCount int
	row = pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_outbox WHERE tenant_id = $1 AND action = 'approval.expired' AND subject_id = $2 AND subject_type = 'pending_transaction'`,
		tid, freshPid)
	if err := row.Scan(&freshCount); err != nil {
		t.Fatalf("query audit_outbox (fresh): %v", err)
	}
	if freshCount != 0 {
		t.Fatalf("approval.expired outbox rows for fresh pending %s = %d, want 0", fresh.ID, freshCount)
	}

	// A second sweep is a no-op: the stale pending is already terminal, and
	// there is nothing left older than the TTL to expire.
	n2, err := approvals.SweepExpiredPending(ctx)
	if err != nil {
		t.Fatalf("second SweepExpiredPending() error = %v, want nil", err)
	}
	if n2 != 0 {
		t.Fatalf("second SweepExpiredPending() = %d, want 0", n2)
	}
}
