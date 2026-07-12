package ledger_test

// Task 5.3 (audit A2.4): streaming verify. AuditService.Verify used to load a
// tenant's ENTIRE chain into memory (ListAuditForVerify) before walking it;
// it now pages through ListAuditForVerifyPage in bounded batches instead.
// These tests force a SMALL page size (via NewAuditServiceWithPageSize) so a
// modest test chain genuinely spans several pages, and prove three things a
// naive rewrite of the paging loop could get wrong: (1) paging actually
// happens (more than one round trip for a chain longer than one page), (2) a
// tamper is caught with the correct FirstBreakID no matter which page it
// falls in, including exactly at a page boundary, and (3) Checked reflects
// the true walk position, not the page size.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// pagingSpyRepo counts calls to ListAuditForVerifyPage while delegating every
// method (including that one) to the embedded real repository. It exists
// solely to prove Verify's paging loop actually issues more than one page
// request for a chain longer than one page, rather than silently reading
// everything in a single oversized call.
type pagingSpyRepo struct {
	domain.Repository
	pageCalls int
}

func (s *pagingSpyRepo) ListAuditForVerifyPage(ctx context.Context, tenantID string, afterChainSeq int64, limit int) ([]domain.AuditEntry, error) {
	s.pageCalls++
	return s.Repository.ListAuditForVerifyPage(ctx, tenantID, afterChainSeq, limit)
}

// postAndDrainChain posts n balanced transactions for tenant through the real
// TransactionService and drains the chainer, returning the resulting
// audit_log rows in chain order (for the caller to pick a row to tamper
// with).
func postAndDrainChain(t *testing.T, pool *pgxpool.Pool, repo *postgres.Repository, tenant, cash, revenue string, n int) []domain.AuditEntry {
	t.Helper()
	ctx := context.Background()
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	for i := 0; i < n; i++ {
		txn := mkTxn(t, cash, revenue)
		if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	drainChainer(t, pool, tenant)
	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("audit rows = %d, want %d", len(rows), n)
	}
	return rows
}

// tamperAuditRow mutates row id's `after` content via a raw UPDATE that
// bypasses the immutability trigger (SET LOCAL audit.allow_purge = 'on'),
// exactly like the existing single-page DetectsTamper test: it leaves
// row_hash untouched, so the row is no longer self-consistent and any walk
// reaching it must break.
func tamperAuditRow(t *testing.T, pool *pgxpool.Pool, rowID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tamper tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL audit.allow_purge = 'on'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_log SET after = $1 WHERE id = $2`,
		[]byte(`{"tampered":true}`), uuid.MustParse(rowID),
	); err != nil {
		t.Fatalf("tamper update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tamper: %v", err)
	}
}

// setupPagedChain creates a tenant with n transactions chained, and an
// AuditService pinned to a small page size so the chain genuinely spans
// several pages, returning the service (wrapping a counting spy), the pool
// (for tampering directly against audit_log), the tenant, and the chain rows
// in order.
func setupPagedChain(t *testing.T, n, pageSize int) (audits *ledger.AuditService, spy *pagingSpyRepo, pool *pgxpool.Pool, tenant string, rows []domain.AuditEntry) { //nolint:unparam // n is a general test-helper parameter; every current caller in this file happens to pass 10
	t.Helper()
	pool = newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	ctx := context.Background()
	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "paged verify test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, cash, nil); err != nil {
		t.Fatalf("create cash: %v", err)
	}
	if err := accounts.Create(ctx, tenant, revenue, nil); err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	rows = postAndDrainChain(t, pool, repo, tenant, cash.ID, revenue.ID, n)

	spy = &pagingSpyRepo{Repository: repo}
	audits = ledger.NewAuditServiceWithPageSize(spy, pageSize)
	return audits, spy, pool, tenant, rows
}

// TestAuditService_Verify_PagesAcrossMultipleCalls proves paging genuinely
// happens (bounded memory, Task 5.3): a 10-row chain read 3 rows at a time
// must take 4 page requests (3, 3, 3, 1), not 1.
func TestAuditService_Verify_PagesAcrossMultipleCalls(t *testing.T) {
	t.Parallel()
	const n, pageSize = 10, 3
	audits, spy, _, tenant, _ := setupPagedChain(t, n, pageSize)

	result, err := audits.Verify(context.Background(), tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != n {
		t.Fatalf("result = %+v, want a valid chain with Checked=%d", result, n)
	}
	wantPages := 4 // ceil(10/3) = 4, with the last page short (1 < pageSize) ending the loop
	if spy.pageCalls != wantPages {
		t.Errorf("page calls = %d, want %d (paging must span multiple requests, not load everything at once)", spy.pageCalls, wantPages)
	}
}

// TestAuditService_Verify_PagedTamper_FirstPage tampers the very first row
// (chain_seq 1, inside page 1 of 4) and checks Verify still reports the
// correct break despite paging.
func TestAuditService_Verify_PagedTamper_FirstPage(t *testing.T) {
	t.Parallel()
	const n, pageSize = 10, 3
	audits, spy, pool, tenant, rows := setupPagedChain(t, n, pageSize)
	target := rows[0] // chain_seq 1

	tamperAuditRow(t, pool, target.ID)

	result, err := audits.Verify(context.Background(), tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid {
		t.Fatal("valid = true after tampering the first row, want false")
	}
	if result.FirstBreakID != target.ID {
		t.Errorf("first break id = %q, want the tampered first row %q", result.FirstBreakID, target.ID)
	}
	if result.Checked != 1 {
		t.Errorf("checked = %d, want 1 (breaks on the very first row of the very first page)", result.Checked)
	}
	if spy.pageCalls != 1 {
		t.Errorf("page calls = %d, want 1 (the break is found within the first page, no further page should be requested)", spy.pageCalls)
	}
}

// TestAuditService_Verify_PagedTamper_MiddlePage tampers a row in the third
// page (chain_seq 7, of 4 pages sized 3/3/3/1), proving a page boundary
// crossed correctly (pages 1 and 2 fully verified, prev correctly carried
// across each page's own boundary) before the break is found.
func TestAuditService_Verify_PagedTamper_MiddlePage(t *testing.T) {
	t.Parallel()
	const n, pageSize = 10, 3
	audits, spy, pool, tenant, rows := setupPagedChain(t, n, pageSize)
	target := rows[6] // chain_seq 7: first row of page 3 (rows 7,8,9)

	tamperAuditRow(t, pool, target.ID)

	result, err := audits.Verify(context.Background(), tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid {
		t.Fatal("valid = true after tampering a middle-page row, want false")
	}
	if result.FirstBreakID != target.ID {
		t.Errorf("first break id = %q, want the tampered row %q", result.FirstBreakID, target.ID)
	}
	if result.Checked != 7 {
		t.Errorf("checked = %d, want 7 (rows 1-6 clean across two full pages, row 7 is the break)", result.Checked)
	}
	if spy.pageCalls != 3 {
		t.Errorf("page calls = %d, want 3 (pages 1 and 2 fully read, the break found on page 3)", spy.pageCalls)
	}
}

// TestAuditService_Verify_PagedTamper_LastPage tampers the very last row
// (chain_seq 10, alone on the fourth, short page), proving the final
// partial page is walked and checked too, not skipped because it is short.
func TestAuditService_Verify_PagedTamper_LastPage(t *testing.T) {
	t.Parallel()
	const n, pageSize = 10, 3
	audits, spy, pool, tenant, rows := setupPagedChain(t, n, pageSize)
	target := rows[9] // chain_seq 10: the lone row of the fourth, short page

	tamperAuditRow(t, pool, target.ID)

	result, err := audits.Verify(context.Background(), tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Valid {
		t.Fatal("valid = true after tampering the last row, want false")
	}
	if result.FirstBreakID != target.ID {
		t.Errorf("first break id = %q, want the tampered last row %q", result.FirstBreakID, target.ID)
	}
	if result.Checked != n {
		t.Errorf("checked = %d, want %d (every row up to and including the last is checked)", result.Checked, n)
	}
	if spy.pageCalls != 4 {
		t.Errorf("page calls = %d, want 4 (all four pages read, the break on the very last row of the last page)", spy.pageCalls)
	}
}
