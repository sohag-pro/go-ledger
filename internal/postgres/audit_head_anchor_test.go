package postgres_test

// Task 4.2 / 5.3 (audit A1.2, A2.4): GetReversalOf, GetAuditHead,
// LatestAuditAnchor, and StatementExport, driven directly against a real
// Postgres. internal/ledger's own tests exercise all four indirectly
// (ReverseTransaction's idempotency precheck, AuditService.Head/LatestAnchor,
// the statement export endpoint), but that is a different package's test
// binary and does not count toward internal/postgres's own coverage.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// seedTxnWithAudit is seedTxn (idempotency_audit_test.go) plus an
// audit_outbox row written in the SAME transaction, mirroring what
// ledger.TransactionService.Post actually does (CreateTransaction AND
// AppendAuditOutbox together, see service.go): a bare repo.CreateTransaction
// call, unlike the real posting path, never touches audit_outbox at all, so
// a test that wants something for the chainer to drain (GetAuditHead,
// LatestAuditAnchor) needs this instead of plain seedTxn.
func seedTxnWithAudit(t *testing.T, repo *postgres.Repository, tenant string) (txnID, debit, credit string) { //nolint:unparam // debit/credit mirror seedTxn's own shape (idempotency_audit_test.go); no current caller here reads them, but the shape stays consistent with seedTxn for future callers
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "test tenant"); err != nil && !errors.Is(err, domain.ErrTenantAlreadyExists) {
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
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: d.ID, Amount: money(t, 500, "USD")},
		{AccountID: c.ID, Amount: money(t, -500, "USD")},
	}}
	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		if err := tx.CreateTransaction(ctx, tenant, txn); err != nil {
			return err
		}
		return tx.AppendAuditOutbox(ctx, tenant, domain.AuditEvent{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txn.ID,
			Actor:         "test",
			After:         []byte(`{}`),
		})
	})
	if err != nil {
		t.Fatalf("create transaction with audit outbox: %v", err)
	}
	return txn.ID, d.ID, c.ID
}

// TestGetReversalOf_FoundAndNotFound covers both branches: no reversal on
// file yet reports domain.ErrTransactionNotFound, and after a reversal is
// posted (a raw RunInTx insert here, standing in for
// TransactionService.ReverseTransaction, since this test is about the
// repository read, not the service that produces the row), GetReversalOf
// returns it with ReversesTransactionID correctly linked back.
func TestGetReversalOf_FoundAndNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	original, debit, credit := seedTxn(t, repo, tenant)

	if _, err := repo.GetReversalOf(ctx, tenant, original); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Fatalf("GetReversalOf before any reversal exists: err = %v, want ErrTransactionNotFound", err)
	}

	reversal := &domain.Transaction{
		ReversesTransactionID: &original,
		Postings: []domain.Posting{
			{AccountID: debit, Amount: money(t, -500, "USD")},
			{AccountID: credit, Amount: money(t, 500, "USD")},
		},
	}
	if err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.CreateTransaction(ctx, tenant, reversal)
	}); err != nil {
		t.Fatalf("create reversal: %v", err)
	}

	got, err := repo.GetReversalOf(ctx, tenant, original)
	if err != nil {
		t.Fatalf("GetReversalOf after a reversal exists: %v", err)
	}
	if got.ID != reversal.ID {
		t.Errorf("GetReversalOf id = %s, want %s", got.ID, reversal.ID)
	}
	if got.ReversesTransactionID == nil || *got.ReversesTransactionID != original {
		t.Errorf("GetReversalOf.ReversesTransactionID = %v, want pointer to %s", got.ReversesTransactionID, original)
	}
	if len(got.Postings) != 2 {
		t.Errorf("GetReversalOf returned %d postings, want 2", len(got.Postings))
	}
}

// TestGetAuditHead_EmptyThenPopulated proves GetAuditHead reports ok=false
// for a tenant with no audit_log rows yet, and after a post is chained,
// reports the correct chain_seq/row_hash matching the chained row itself.
func TestGetAuditHead_EmptyThenPopulated(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	chainSeq, rowHash, ok, err := repo.GetAuditHead(ctx, tenant)
	if err != nil {
		t.Fatalf("GetAuditHead (empty chain): %v", err)
	}
	if ok {
		t.Fatalf("GetAuditHead (empty chain): ok = true, want false (chainSeq=%d rowHash=%q)", chainSeq, rowHash)
	}

	txnID, _, _ := seedTxnWithAudit(t, repo, tenant)
	drainChainer(t, pool, tenant)

	// ListAuditForVerifyPage (unlike ListAuditForVerify) populates ChainSeq,
	// so it is what this test needs to compare against GetAuditHead's own
	// return value.
	rows, err := repo.ListAuditForVerifyPage(ctx, tenant, 0, 10)
	if err != nil {
		t.Fatalf("list audit for verify page: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows after one post = %d, want 1", len(rows))
	}
	if rows[0].TransactionID != txnID {
		t.Errorf("audit row transaction id = %s, want %s", rows[0].TransactionID, txnID)
	}

	chainSeq, rowHash, ok, err = repo.GetAuditHead(ctx, tenant)
	if err != nil {
		t.Fatalf("GetAuditHead (populated): %v", err)
	}
	if !ok {
		t.Fatal("GetAuditHead (populated): ok = false, want true")
	}
	if chainSeq != rows[0].ChainSeq {
		t.Errorf("GetAuditHead chainSeq = %d, want %d", chainSeq, rows[0].ChainSeq)
	}
	if rowHash != rows[0].RowHash {
		t.Errorf("GetAuditHead rowHash = %q, want %q", rowHash, rows[0].RowHash)
	}
}

// TestLatestAuditAnchor_NoneThenRecorded proves LatestAuditAnchor reports
// ok=false for a tenant with no anchor ever recorded, and after
// internal/audit.AnchorJob.AnchorOnce runs, reports the anchored chain_seq
// and row_hash matching the tenant's chain head at that point.
func TestLatestAuditAnchor_NoneThenRecorded(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	_, ok, err := repo.LatestAuditAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("LatestAuditAnchor (none recorded): %v", err)
	}
	if ok {
		t.Fatal("LatestAuditAnchor (none recorded): ok = true, want false")
	}

	_, _, _ = seedTxnWithAudit(t, repo, tenant)
	drainChainer(t, pool, tenant)

	headSeq, headHash, headOK, err := repo.GetAuditHead(ctx, tenant)
	if err != nil || !headOK {
		t.Fatalf("get audit head before anchoring: ok=%v err=%v", headOK, err)
	}

	anchorJob := audit.NewAnchorJob(pool, discardTestLogger(), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}

	anchor, ok, err := repo.LatestAuditAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("LatestAuditAnchor (recorded): %v", err)
	}
	if !ok {
		t.Fatal("LatestAuditAnchor (recorded): ok = false, want true")
	}
	if anchor.ChainSeq != headSeq {
		t.Errorf("anchor chain_seq = %d, want %d", anchor.ChainSeq, headSeq)
	}
	if anchor.RowHash != headHash {
		t.Errorf("anchor row_hash = %q, want %q", anchor.RowHash, headHash)
	}
	if anchor.CreatedAt.IsZero() {
		t.Error("anchor created_at is zero, want a real timestamp")
	}
}

// TestStatementExport_WindowAndUnbounded proves StatementExport (Task 6.3,
// audit A9.2) returns entries newest-first with correct running balances both
// unbounded (from/to both nil) and restricted to a [from, to) window that
// excludes some postings.
func TestStatementExport_WindowAndUnbounded(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "statement export repo test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	other := &domain.Account{Name: "Other", Type: domain.Asset, Currency: "USD"}
	for _, a := range []*domain.Account{cash, other} {
		if err := repo.CreateAccount(ctx, tenant, a); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}

	// Three deposits of 100 into cash: running balances 100, 200, 300.
	for i := 0; i < 3; i++ {
		txn := &domain.Transaction{Postings: []domain.Posting{
			{AccountID: cash.ID, Amount: money(t, 100, "USD")},
			{AccountID: other.ID, Amount: money(t, -100, "USD")},
		}}
		if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}

	// Unbounded: from and to both nil returns every entry, newest first.
	all, err := repo.StatementExport(ctx, tenant, cash.ID, "USD", nil, nil, 100)
	if err != nil {
		t.Fatalf("statement export (unbounded): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("statement export (unbounded) = %d entries, want 3", len(all))
	}
	if all[0].RunningBalance.Amount() != 300 || all[2].RunningBalance.Amount() != 100 {
		t.Errorf("statement export (unbounded) running balances = %d,%d,%d, want 300,200,100",
			all[0].RunningBalance.Amount(), all[1].RunningBalance.Amount(), all[2].RunningBalance.Amount())
	}

	// A [from, to) window that only covers the middle entry's created_at:
	// bracket it tightly using its own timestamp.
	from := all[1].CreatedAt
	to := all[0].CreatedAt
	windowed, err := repo.StatementExport(ctx, tenant, cash.ID, "USD", &from, &to, 100)
	if err != nil {
		t.Fatalf("statement export (windowed): %v", err)
	}
	if len(windowed) != 1 {
		t.Fatalf("statement export (windowed [%s,%s)) = %d entries, want 1", from, to, len(windowed))
	}
	if windowed[0].ID != all[1].ID {
		t.Errorf("statement export (windowed) returned entry %s, want %s", windowed[0].ID, all[1].ID)
	}

	// A limit smaller than the total row count truncates rather than erroring.
	limited, err := repo.StatementExport(ctx, tenant, cash.ID, "USD", nil, nil, 2)
	if err != nil {
		t.Fatalf("statement export (limit 2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("statement export (limit 2) = %d entries, want 2", len(limited))
	}
}

// TestStatementExport_MalformedIDs proves StatementExport fails closed with a
// parse error for a syntactically invalid tenant or account id.
func TestStatementExport_MalformedIDs(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	const bad = "not-a-uuid"

	if _, err := repo.StatementExport(ctx, bad, uuid.NewString(), "USD", nil, nil, 10); err == nil {
		t.Error("StatementExport(bad tenant): expected a parse error, got nil")
	}
	if _, err := repo.StatementExport(ctx, uuid.NewString(), bad, "USD", nil, nil, 10); err == nil {
		t.Error("StatementExport(bad account): expected a parse error, got nil")
	}
}

// TestGetAuditHeadAndLatestAuditAnchor_MalformedTenantID proves both reads
// fail closed with a parse error for a syntactically invalid tenant id.
func TestGetAuditHeadAndLatestAuditAnchor_MalformedTenantID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	const bad = "not-a-uuid"

	if _, _, _, err := repo.GetAuditHead(ctx, bad); err == nil {
		t.Error("GetAuditHead(bad tenant): expected a parse error, got nil")
	}
	if _, _, err := repo.LatestAuditAnchor(ctx, bad); err == nil {
		t.Error("LatestAuditAnchor(bad tenant): expected a parse error, got nil")
	}
	if _, err := repo.GetReversalOf(ctx, bad, uuid.NewString()); err == nil {
		t.Error("GetReversalOf(bad tenant): expected a parse error, got nil")
	}
	if _, err := repo.GetReversalOf(ctx, uuid.NewString(), bad); err == nil {
		t.Error("GetReversalOf(bad original id): expected a parse error, got nil")
	}
}
