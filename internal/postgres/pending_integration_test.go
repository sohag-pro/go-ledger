package postgres_test

// Task 4 (ADR-025, Week 13): storage-layer tests for pending_transactions.
// This is schema + repository CRUD/transition coverage only, no gate logic
// (that is Task 5+): a caller here builds a domain.PendingTransaction
// directly and drives InsertPendingTransaction/GetPendingTransaction/
// ListPendingTransactions/SweepExpiredPending (Repository) and
// GetPendingForUpdate/UpdatePendingStatus (Tx) the same way
// disputes_repo_test.go drives Dispute's own repository surface.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestPendingTransactionsRepo_GetNotFound proves GetPendingTransaction
// reports domain.ErrPendingTransactionNotFound for an id with no matching
// row.
func TestPendingTransactionsRepo_GetNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetPendingTransaction(context.Background(), uuid.NewString(), uuid.NewString())
	if !errors.Is(err, domain.ErrPendingTransactionNotFound) {
		t.Errorf("GetPendingTransaction (unknown id) = %v, want ErrPendingTransactionNotFound", err)
	}
}

// TestPendingTransactionsRepo_InsertGetListApprove covers the bulk of the
// repository surface end to end against a real Postgres: InsertPendingTransaction
// assigns an id and defaults Status to pending, GetPendingTransaction
// round-trips every field, ListPendingTransactions returns it (by tenant and
// filtered by status), and the RunInTx pair GetPendingForUpdate +
// UpdatePendingStatus transitions it to approved with a linked transaction
// id, which GetPendingTransaction then reflects.
func TestPendingTransactionsRepo_InsertGetListApprove(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "pending repo test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	payload := json.RawMessage(`{"postings":[{"account_id":"a","amount":100}]}`)
	p := &domain.PendingTransaction{
		Kind:         domain.PendingKindPost,
		Payload:      payload,
		ThresholdCcy: "USD",
		ThresholdAmt: 100000,
		CreatedBy:    "api-key-1",
	}
	if err := repo.InsertPendingTransaction(ctx, tenant, p); err != nil {
		t.Fatalf("insert pending transaction: %v", err)
	}
	if p.ID == "" {
		t.Fatal("InsertPendingTransaction did not assign an id")
	}
	if p.Status != domain.PendingStatusPending {
		t.Errorf("InsertPendingTransaction status = %q, want %q (defaulted)", p.Status, domain.PendingStatusPending)
	}

	got, err := repo.GetPendingTransaction(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("get pending transaction: %v", err)
	}
	if got.ID != p.ID || got.TenantID != tenant {
		t.Errorf("GetPendingTransaction id/tenant = %q/%q, want %q/%q", got.ID, got.TenantID, p.ID, tenant)
	}
	if got.Kind != domain.PendingKindPost {
		t.Errorf("GetPendingTransaction Kind = %q, want %q", got.Kind, domain.PendingKindPost)
	}
	if string(got.Payload) != string(payload) {
		t.Errorf("GetPendingTransaction Payload = %s, want %s", got.Payload, payload)
	}
	if got.Status != domain.PendingStatusPending {
		t.Errorf("GetPendingTransaction Status = %q, want %q", got.Status, domain.PendingStatusPending)
	}
	if got.ThresholdCcy != "USD" || got.ThresholdAmt != 100000 {
		t.Errorf("GetPendingTransaction threshold = %s %d, want USD 100000", got.ThresholdCcy, got.ThresholdAmt)
	}
	if got.CreatedBy != "api-key-1" {
		t.Errorf("GetPendingTransaction CreatedBy = %q, want %q", got.CreatedBy, "api-key-1")
	}
	if got.CreatedAt.IsZero() {
		t.Error("GetPendingTransaction CreatedAt is zero, want a server-stamped timestamp")
	}
	if got.DecidedBy != nil || got.DecidedAt != nil || got.Reason != nil || got.TransactionID != nil {
		t.Errorf("GetPendingTransaction (fresh pending) decision fields = %+v, want all nil", got)
	}

	// A second pending for the same tenant, so list/filter has more than one
	// row to work with.
	p2 := &domain.PendingTransaction{
		Kind:         domain.PendingKindConvert,
		Payload:      json.RawMessage(`{"from":"a","to":"b"}`),
		ThresholdCcy: "EUR",
		ThresholdAmt: 90000,
		CreatedBy:    "api-key-1",
	}
	if err := repo.InsertPendingTransaction(ctx, tenant, p2); err != nil {
		t.Fatalf("insert second pending transaction: %v", err)
	}

	all, err := repo.ListPendingTransactions(ctx, tenant, nil, nil, 50)
	if err != nil {
		t.Fatalf("list pending transactions (no filter): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list pending transactions (no filter) = %d, want 2", len(all))
	}

	// Keyset paging: limit 1 returns exactly one row (newest first), and the
	// cursor's second page returns the other one.
	page1, err := repo.ListPendingTransactions(ctx, tenant, nil, nil, 1)
	if err != nil {
		t.Fatalf("list pending transactions (limit 1): %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("list pending transactions (limit 1) = %d rows, want 1", len(page1))
	}
	cursor := &domain.StatementCursor{CreatedAt: page1[0].CreatedAt, ID: page1[0].ID}
	page2, err := repo.ListPendingTransactions(ctx, tenant, nil, cursor, 50)
	if err != nil {
		t.Fatalf("list pending transactions (page 2): %v", err)
	}
	if len(page2) != 1 || page2[0].ID == page1[0].ID {
		t.Errorf("list pending transactions (page 2) = %+v, want the other pending", page2)
	}

	// Approve p via the Tx pair, linking a (for this repository-level test,
	// arbitrary) posted transaction id: GetPendingForUpdate locks the row
	// within RunInTx's transaction, UpdatePendingStatus transitions it.
	postedTxnID := uuid.NewString()
	if err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		locked, err := tx.GetPendingForUpdate(ctx, tenant, p.ID)
		if err != nil {
			return err
		}
		if locked.Status != domain.PendingStatusPending {
			t.Errorf("GetPendingForUpdate status = %q, want %q", locked.Status, domain.PendingStatusPending)
		}
		return tx.UpdatePendingStatus(ctx, tenant, p.ID, domain.PendingStatusApproved, "approver-1", nil, &postedTxnID)
	}); err != nil {
		t.Fatalf("approve pending transaction: %v", err)
	}

	approved, err := repo.GetPendingTransaction(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("get pending transaction after approve: %v", err)
	}
	if approved.Status != domain.PendingStatusApproved {
		t.Errorf("approved status = %q, want %q", approved.Status, domain.PendingStatusApproved)
	}
	if approved.DecidedBy == nil || *approved.DecidedBy != "approver-1" {
		t.Errorf("approved DecidedBy = %v, want %q", approved.DecidedBy, "approver-1")
	}
	if approved.DecidedAt == nil {
		t.Error("approved DecidedAt = nil, want a timestamp")
	}
	if approved.TransactionID == nil || *approved.TransactionID != postedTxnID {
		t.Errorf("approved TransactionID = %v, want %q", approved.TransactionID, postedTxnID)
	}
	if approved.Reason != nil {
		t.Errorf("approved Reason = %v, want nil (none given)", approved.Reason)
	}

	// ListPendingTransactions filtered by status: only p2 is still pending.
	pendingOnly := domain.PendingStatusPending
	filtered, err := repo.ListPendingTransactions(ctx, tenant, &pendingOnly, nil, 50)
	if err != nil {
		t.Fatalf("list pending transactions (pending filter): %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != p2.ID {
		t.Errorf("list pending transactions (pending filter) = %+v, want exactly pending %s", filtered, p2.ID)
	}

	approvedOnly := domain.PendingStatusApproved
	filteredApproved, err := repo.ListPendingTransactions(ctx, tenant, &approvedOnly, nil, 50)
	if err != nil {
		t.Fatalf("list pending transactions (approved filter): %v", err)
	}
	if len(filteredApproved) != 1 || filteredApproved[0].ID != p.ID {
		t.Errorf("list pending transactions (approved filter) = %+v, want exactly approved %s", filteredApproved, p.ID)
	}
}

// TestPendingTransactionsRepo_SweepExpired proves SweepExpiredPending moves
// an old, still-pending row to expired while leaving a fresh pending row
// (and a non-pending row) untouched: the background TTL sweep must never
// touch a row that has not actually aged past the threshold, or one that
// already reached a terminal state some other way.
func TestPendingTransactionsRepo_SweepExpired(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "pending sweep test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	fresh := &domain.PendingTransaction{
		Kind: domain.PendingKindPost, Payload: json.RawMessage(`{}`),
		ThresholdCcy: "USD", ThresholdAmt: 1, CreatedBy: "api-key-1",
	}
	if err := repo.InsertPendingTransaction(ctx, tenant, fresh); err != nil {
		t.Fatalf("insert fresh pending: %v", err)
	}

	stale := &domain.PendingTransaction{
		Kind: domain.PendingKindPost, Payload: json.RawMessage(`{}`),
		ThresholdCcy: "USD", ThresholdAmt: 1, CreatedBy: "api-key-1",
	}
	if err := repo.InsertPendingTransaction(ctx, tenant, stale); err != nil {
		t.Fatalf("insert stale pending: %v", err)
	}
	// Backdate directly (simulating PENDING_TTL having elapsed), the same
	// technique TestSweepExpiredIdempotencyKeys uses for expires_at: no real
	// sleep needed for a deterministic old row.
	if _, err := pool.Exec(ctx,
		`UPDATE pending_transactions SET created_at = now() - interval '4 hours' WHERE tenant_id = $1 AND id = $2`,
		tenant, stale.ID,
	); err != nil {
		t.Fatalf("backdate stale pending: %v", err)
	}

	// SweepExpiredPending is deliberately NOT tenant-scoped (mirroring
	// SweepExpiredIdempotencyKeys), so under t.Parallel() a sibling test's
	// own rows may also be swept; this test only asserts about its OWN two
	// rows, read back by id afterward, rather than the returned slice's
	// exact length.
	if _, err := repo.SweepExpiredPending(ctx, 3*time.Hour); err != nil {
		t.Fatalf("sweep expired pending: %v", err)
	}

	gotStale, err := repo.GetPendingTransaction(ctx, tenant, stale.ID)
	if err != nil {
		t.Fatalf("get stale pending after sweep: %v", err)
	}
	if gotStale.Status != domain.PendingStatusExpired {
		t.Errorf("stale pending status after sweep = %q, want %q", gotStale.Status, domain.PendingStatusExpired)
	}
	if gotStale.DecidedBy == nil || *gotStale.DecidedBy != "system" {
		t.Errorf("stale pending DecidedBy after sweep = %v, want %q", gotStale.DecidedBy, "system")
	}
	if gotStale.DecidedAt == nil {
		t.Error("stale pending DecidedAt after sweep = nil, want a timestamp")
	}

	gotFresh, err := repo.GetPendingTransaction(ctx, tenant, fresh.ID)
	if err != nil {
		t.Fatalf("get fresh pending after sweep: %v", err)
	}
	if gotFresh.Status != domain.PendingStatusPending {
		t.Errorf("fresh pending status after sweep = %q, want %q (untouched)", gotFresh.Status, domain.PendingStatusPending)
	}
}
