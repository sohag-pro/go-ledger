package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestAuditService_Verify_EmptyChain checks a tenant with no audit rows at all
// verifies as valid with nothing checked: there is nothing to break.
func TestAuditService_Verify_EmptyChain(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()

	result, err := audits.Verify(ctx, uuid.NewString())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid || result.Checked != 0 || result.FirstBreakID != "" || result.Pending != 0 {
		t.Errorf("result = %+v, want {Valid:true Checked:0 FirstBreakID:\"\" Pending:0}", result)
	}
}

// TestAuditService_Verify_ValidChain posts several real transactions (each one
// transactionally extends the tenant's audit chain, per ADR-012) and checks
// Verify walks the whole chain and reports it valid, having checked every row.
func TestAuditService_Verify_ValidChain(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "audit verify test tenant"); err != nil {
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

	const n = 3
	for i := 0; i < n; i++ {
		txn := mkTxn(t, cash.ID, revenue.ID)
		if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	// Post only writes an audit_outbox row (ADR-017); drain the chainer so
	// the chain actually exists before walking it.
	drainChainer(t, pool, tenant)

	result, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Errorf("valid = false, want true")
	}
	if result.Checked != n {
		t.Errorf("checked = %d, want %d", result.Checked, n)
	}
	if result.FirstBreakID != "" {
		t.Errorf("first break id = %q, want empty on a valid chain", result.FirstBreakID)
	}
	if result.Pending != 0 {
		t.Errorf("pending = %d, want 0 after draining", result.Pending)
	}
}

// TestAuditService_Verify_DetectsTamper posts three transactions, then tampers
// with the middle audit row's `after` content via a raw UPDATE that bypasses
// the immutability trigger (the seeder's own escape hatch, SET LOCAL
// audit.allow_purge = 'on'), exactly like the postgres-level hash chain tests.
// Verify must report the chain invalid, stop at the tampered row, and report
// it as the first break: the row's stored row_hash no longer matches what its
// (now-altered) content recomputes to.
func TestAuditService_Verify_DetectsTamper(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "audit verify test tenant"); err != nil {
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

	const n = 3
	for i := 0; i < n; i++ {
		txn := mkTxn(t, cash.ID, revenue.ID)
		if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	// Post only writes an audit_outbox row (ADR-017); drain the chainer so
	// there is an audit_log row to tamper with.
	drainChainer(t, pool, tenant)

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("audit rows = %d, want %d", len(rows), n)
	}
	middle := rows[1]

	// Sanity: the chain verifies clean before any tampering.
	if before, err := audits.Verify(ctx, tenant); err != nil || !before.Valid {
		t.Fatalf("verify before tampering: result=%+v err=%v, want a valid chain", before, err)
	}

	tamperTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tamper tx: %v", err)
	}
	defer tamperTx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if _, err := tamperTx.Exec(ctx, `SET LOCAL audit.allow_purge = 'on'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := tamperTx.Exec(ctx,
		`UPDATE audit_log SET after = $1 WHERE id = $2`,
		[]byte(`{"tampered":true}`), uuid.MustParse(middle.ID),
	); err != nil {
		t.Fatalf("tamper update: %v", err)
	}
	if err := tamperTx.Commit(ctx); err != nil {
		t.Fatalf("commit tamper: %v", err)
	}

	result, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify after tampering: %v", err)
	}
	if result.Valid {
		t.Fatal("valid = true after tampering, want false")
	}
	if result.FirstBreakID != middle.ID {
		t.Errorf("first break id = %q, want the tampered row %q", result.FirstBreakID, middle.ID)
	}
	// The walk stops at the tampered row itself: rows[0] still checks out, so
	// Checked must be exactly 2 (row 0 passing, row 1 the break), not 1 or 3.
	if result.Checked != 2 {
		t.Errorf("checked = %d, want 2 (stops at the tampered row)", result.Checked)
	}
}
