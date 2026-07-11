package postgres_test

// Task 6.3 / audit A9.2: these tests drive internal/postgres/disputes.go's
// repository methods (GetDispute, ListDisputes, ResolveDispute; CreateDispute
// itself already has direct coverage via migration0029_test.go) DIRECTLY,
// bypassing internal/ledger.DisputeService entirely. internal/ledger's own
// dispute_service_test.go exercises the same underlying rows, but coverage is
// per package: a call reaching this code through a different package's test
// binary is never counted toward internal/postgres's own coverage.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestDisputesRepo_GetNotFound proves GetDispute reports
// domain.ErrDisputeNotFound for an id with no matching row.
func TestDisputesRepo_GetNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetDispute(context.Background(), uuid.NewString(), uuid.NewString())
	if !errors.Is(err, domain.ErrDisputeNotFound) {
		t.Errorf("GetDispute (unknown id) = %v, want ErrDisputeNotFound", err)
	}
}

// TestDisputesRepo_CreateGetListResolve covers the whole repository surface
// end to end against a real Postgres: CreateDispute assigns an id and
// defaults Status to open, GetDispute round-trips it, ListDisputes returns it
// (optionally filtered by status), and ResolveDispute both rejects (no
// resolution transaction) and reverses (with one) correctly transition the
// row and stamp resolved_at.
func TestDisputesRepo_CreateGetListResolve(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, tenant)

	d := &domain.Dispute{TransactionID: txnID, Reason: "customer chargeback"}
	if err := repo.CreateDispute(ctx, tenant, d); err != nil {
		t.Fatalf("create dispute: %v", err)
	}
	if d.ID == "" {
		t.Fatal("CreateDispute did not assign an id")
	}
	if d.Status != domain.DisputeOpen {
		t.Errorf("CreateDispute status = %q, want %q (defaulted)", d.Status, domain.DisputeOpen)
	}

	got, err := repo.GetDispute(ctx, tenant, d.ID)
	if err != nil {
		t.Fatalf("get dispute: %v", err)
	}
	if got.TransactionID != txnID || got.Reason != "customer chargeback" || got.Status != domain.DisputeOpen {
		t.Errorf("GetDispute = %+v, want transaction %s, reason set, status open", got, txnID)
	}
	if got.ResolvedAt != nil {
		t.Errorf("GetDispute.ResolvedAt = %v, want nil for an open dispute", got.ResolvedAt)
	}

	// A second dispute against a second transaction, so ListDisputes has more
	// than one row to page and filter over.
	txnID2, _, _ := seedTxn(t, repo, tenant)
	d2 := &domain.Dispute{TransactionID: txnID2, Reason: "second dispute"}
	if err := repo.CreateDispute(ctx, tenant, d2); err != nil {
		t.Fatalf("create second dispute: %v", err)
	}

	all, err := repo.ListDisputes(ctx, tenant, nil, nil, 50)
	if err != nil {
		t.Fatalf("list disputes (no filter): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list disputes (no filter) = %d, want 2", len(all))
	}

	// Resolve the first as rejected: no money moves, no resolution
	// transaction id, status resolved_rejected, resolved_at stamped.
	rejected, err := repo.ResolveDispute(ctx, tenant, d.ID, domain.DisputeResolvedRejected, nil)
	if err != nil {
		t.Fatalf("resolve dispute (reject): %v", err)
	}
	if rejected.Status != domain.DisputeResolvedRejected {
		t.Errorf("resolved status = %q, want %q", rejected.Status, domain.DisputeResolvedRejected)
	}
	if rejected.ResolutionTransactionID != nil {
		t.Errorf("resolved (reject) resolution_transaction_id = %v, want nil", rejected.ResolutionTransactionID)
	}
	if rejected.ResolvedAt == nil {
		t.Error("resolved (reject) resolved_at = nil, want a timestamp")
	}

	// Resolve the second as reversed, linking a real (if arbitrary, for this
	// repository-level test) resolution transaction id: ResolveDispute itself
	// does not validate that the reversal is real, that guarantee lives one
	// layer up in ledger.DisputeService.Resolve, which always calls
	// ReverseTransaction first (see dispute_service_test.go).
	reversalTxnID, _, _ := seedTxn(t, repo, tenant)
	reversed, err := repo.ResolveDispute(ctx, tenant, d2.ID, domain.DisputeResolvedReversed, &reversalTxnID)
	if err != nil {
		t.Fatalf("resolve dispute (reverse): %v", err)
	}
	if reversed.Status != domain.DisputeResolvedReversed {
		t.Errorf("resolved status = %q, want %q", reversed.Status, domain.DisputeResolvedReversed)
	}
	if reversed.ResolutionTransactionID == nil || *reversed.ResolutionTransactionID != reversalTxnID {
		t.Errorf("resolved (reverse) resolution_transaction_id = %v, want %q", reversed.ResolutionTransactionID, reversalTxnID)
	}

	// ListDisputes filtered by status: only the still-untouched... there are
	// none left open now, so filter by the two terminal statuses instead.
	rejectedOnly := domain.DisputeResolvedRejected
	filtered, err := repo.ListDisputes(ctx, tenant, &rejectedOnly, nil, 50)
	if err != nil {
		t.Fatalf("list disputes (rejected filter): %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != d.ID {
		t.Errorf("list disputes (rejected filter) = %+v, want exactly dispute %s", filtered, d.ID)
	}

	// Keyset paging: limit 1 returns exactly one row (newest first).
	page1, err := repo.ListDisputes(ctx, tenant, nil, nil, 1)
	if err != nil {
		t.Fatalf("list disputes (limit 1): %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("list disputes (limit 1) = %d rows, want 1", len(page1))
	}
	cursor := &domain.StatementCursor{CreatedAt: page1[0].CreatedAt, ID: page1[0].ID}
	page2, err := repo.ListDisputes(ctx, tenant, nil, cursor, 50)
	if err != nil {
		t.Fatalf("list disputes (page 2): %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("list disputes (page 2) = %d rows, want 1 (the remaining dispute)", len(page2))
	}
	if page2[0].ID == page1[0].ID {
		t.Error("list disputes (page 2) returned the same dispute as page 1, want the other one")
	}
}

// TestDisputesRepo_ResolveAlreadyResolvedVsNotFound proves ResolveDispute
// tells apart "no such dispute at all" (ErrDisputeNotFound) from "exists but
// is no longer open" (ErrDisputeAlreadyResolved): the guarded UPDATE alone
// cannot distinguish the two (both affect zero rows), so ResolveDispute falls
// back to a GetDispute to decide which sentinel to report.
func TestDisputesRepo_ResolveAlreadyResolvedVsNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, tenant)

	d := &domain.Dispute{TransactionID: txnID, Reason: "will be resolved twice"}
	if err := repo.CreateDispute(ctx, tenant, d); err != nil {
		t.Fatalf("create dispute: %v", err)
	}
	if _, err := repo.ResolveDispute(ctx, tenant, d.ID, domain.DisputeResolvedRejected, nil); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Second resolve attempt against an already-terminal dispute.
	if _, err := repo.ResolveDispute(ctx, tenant, d.ID, domain.DisputeResolvedRejected, nil); !errors.Is(err, domain.ErrDisputeAlreadyResolved) {
		t.Errorf("second resolve on an already-resolved dispute: err = %v, want ErrDisputeAlreadyResolved", err)
	}

	// An id that never existed at all.
	if _, err := repo.ResolveDispute(ctx, tenant, uuid.NewString(), domain.DisputeResolvedRejected, nil); !errors.Is(err, domain.ErrDisputeNotFound) {
		t.Errorf("resolve on an unknown dispute id: err = %v, want ErrDisputeNotFound", err)
	}
}

// TestDisputesRepo_MalformedIDs proves CreateDispute, GetDispute,
// ListDisputes, and ResolveDispute all fail closed with a parse error for a
// syntactically invalid id, mirroring TestMalformedIDsReturnErrors
// (coverage_test.go) for the rest of the repository surface.
func TestDisputesRepo_MalformedIDs(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	const bad = "not-a-uuid"

	tests := []struct {
		name string
		call func() error
	}{
		{"CreateDispute bad tenant", func() error {
			return repo.CreateDispute(ctx, bad, &domain.Dispute{TransactionID: uuid.NewString(), Reason: "x"})
		}},
		{"CreateDispute bad preset id", func() error {
			return repo.CreateDispute(ctx, uuid.NewString(), &domain.Dispute{ID: bad, TransactionID: uuid.NewString(), Reason: "x"})
		}},
		{"CreateDispute bad transaction id", func() error {
			return repo.CreateDispute(ctx, uuid.NewString(), &domain.Dispute{TransactionID: bad, Reason: "x"})
		}},
		{"GetDispute bad tenant", func() error {
			_, err := repo.GetDispute(ctx, bad, uuid.NewString())
			return err
		}},
		{"GetDispute bad id", func() error {
			_, err := repo.GetDispute(ctx, uuid.NewString(), bad)
			return err
		}},
		{"ListDisputes bad tenant", func() error {
			_, err := repo.ListDisputes(ctx, bad, nil, nil, 10)
			return err
		}},
		{"ListDisputes bad cursor id", func() error {
			cursor := &domain.StatementCursor{ID: bad}
			_, err := repo.ListDisputes(ctx, uuid.NewString(), nil, cursor, 10)
			return err
		}},
		{"ResolveDispute bad tenant", func() error {
			_, err := repo.ResolveDispute(ctx, bad, uuid.NewString(), domain.DisputeResolvedRejected, nil)
			return err
		}},
		{"ResolveDispute bad id", func() error {
			_, err := repo.ResolveDispute(ctx, uuid.NewString(), bad, domain.DisputeResolvedRejected, nil)
			return err
		}},
		{"ResolveDispute bad resolution transaction id", func() error {
			badResolutionID := bad
			_, err := repo.ResolveDispute(ctx, uuid.NewString(), uuid.NewString(), domain.DisputeResolvedReversed, &badResolutionID)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); err == nil {
				t.Fatal("expected a parse error, got nil")
			}
		})
	}
}
