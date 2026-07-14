package ledger_test

// Task 5.3 (audit A2.4): VerifyFromLatestAnchor's from-anchor fast path.
// Unlike Verify (always from genesis), this walks only the tail past the
// tenant's most recently recorded off-box anchor, bounding cost to growth
// since that anchor rather than total chain length. These tests use the
// real internal/audit.AnchorJob to record anchors (AnchorOnce, no leader
// election needed here), exactly as the production anchor job would.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestAuditService_VerifyFromLatestAnchor_NoAnchorFallsBack proves that with
// no anchor ever recorded, VerifyFromLatestAnchor behaves exactly like a
// full Verify from genesis: same Valid, same Checked (the whole chain).
func TestAuditService_VerifyFromLatestAnchor_NoAnchorFallsBack(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "from-anchor fallback test tenant"); err != nil {
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
	const n = 4
	postAndDrainChain(t, pool, repo, tenant, cash.ID, revenue.ID, n)

	// Sanity: no anchor recorded yet for this brand-new tenant.
	if _, ok, err := repo.LatestAuditAnchor(ctx, tenant); err != nil {
		t.Fatalf("latest audit anchor: %v", err)
	} else if ok {
		t.Fatal("test setup bug: an anchor already exists for a brand-new tenant")
	}

	full, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	fromAnchor, err := audits.VerifyFromLatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("verify from latest anchor: %v", err)
	}
	if fromAnchor != full {
		t.Errorf("verify from latest anchor (no anchor) = %+v, want it identical to a full verify %+v", fromAnchor, full)
	}
	if !fromAnchor.Valid || fromAnchor.Checked != n {
		t.Errorf("result = %+v, want a valid chain with Checked=%d", fromAnchor, n)
	}
}

// TestAuditService_VerifyFromLatestAnchor_BoundedToTail anchors the chain at
// chain_seq K, posts more rows afterward, and proves VerifyFromLatestAnchor
// only reads (and only counts, in Checked) the tail past K, not the whole
// chain.
func TestAuditService_VerifyFromLatestAnchor_BoundedToTail(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "from-anchor bounded test tenant"); err != nil {
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

	const beforeAnchor, afterAnchor = 5, 3
	postAndDrainChain(t, pool, repo, tenant, cash.ID, revenue.ID, beforeAnchor)

	// chain_seq is a single global sequence across every tenant (ADR-017,
	// migration 0016), not a per-tenant counter restarting at 1: this test
	// shares the package's container with every other test, so the tenant's
	// own head chain_seq at this point is whatever the global sequence has
	// reached, not literally beforeAnchor. Read it back rather than assuming
	// a specific number.
	wantHeadSeq, wantHeadHash, ok, err := repo.GetAuditHead(ctx, tenant)
	if err != nil {
		t.Fatalf("get audit head before anchoring: %v", err)
	}
	if !ok {
		t.Fatalf("get audit head before anchoring: no head, want one after posting %d rows", beforeAnchor)
	}

	anchorJob := audit.NewAnchorJob(pool, discardLogger(), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}
	anchor, ok, err := repo.LatestAuditAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("latest audit anchor: %v", err)
	}
	if !ok || anchor.ChainSeq != wantHeadSeq || anchor.RowHash != wantHeadHash {
		t.Fatalf("anchor = %+v (ok=%v), want chain_seq=%d row_hash=%q (this tenant's head at anchor time)",
			anchor, ok, wantHeadSeq, wantHeadHash)
	}

	// Extend the chain past the anchor.
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	for i := 0; i < afterAnchor; i++ {
		txn := mkTxn(t, cash.ID, revenue.ID)
		if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("post tail %d: %v", i, err)
		}
	}
	drainChainer(t, pool, tenant)

	full, err := audits.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !full.Valid || full.Checked != beforeAnchor+afterAnchor {
		t.Fatalf("full verify = %+v, want Checked=%d", full, beforeAnchor+afterAnchor)
	}

	fromAnchor, err := audits.VerifyFromLatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("verify from latest anchor: %v", err)
	}
	if !fromAnchor.Valid {
		t.Fatalf("verify from latest anchor: valid = false, want true")
	}
	if fromAnchor.Checked != afterAnchor {
		t.Errorf("verify from latest anchor: checked = %d, want %d (only the tail past the anchor, not the full chain of %d)",
			fromAnchor.Checked, afterAnchor, beforeAnchor+afterAnchor)
	}
}

// TestAuditService_VerifyFromLatestAnchor_CatchesTailTamper anchors the
// chain, extends it, tampers one of the NEW tail rows (the ordinary,
// self-inconsistent kind of tamper: content changed, row_hash left as-is),
// and checks VerifyFromLatestAnchor still catches it with the correct
// FirstBreakID even though it never re-walks anything at or before the
// anchor.
func TestAuditService_VerifyFromLatestAnchor_CatchesTailTamper(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	audits := ledger.NewAuditService(repo)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "from-anchor tail tamper test tenant"); err != nil {
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

	const beforeAnchor = 4
	postAndDrainChain(t, pool, repo, tenant, cash.ID, revenue.ID, beforeAnchor)

	anchorJob := audit.NewAnchorJob(pool, discardLogger(), time.Hour)
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}

	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	const afterAnchor = 3
	for i := 0; i < afterAnchor; i++ {
		txn := mkTxn(t, cash.ID, revenue.ID)
		if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
			t.Fatalf("post tail %d: %v", i, err)
		}
	}
	drainChainer(t, pool, tenant)

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != beforeAnchor+afterAnchor {
		t.Fatalf("audit rows = %d, want %d", len(rows), beforeAnchor+afterAnchor)
	}
	// The second tail row (chain_seq beforeAnchor+2): a middle-of-tail
	// tamper, not the tail's own first or last row, so the from-anchor walk
	// must both start correctly at the anchor AND continue past one clean
	// tail row before finding the break.
	target := rows[beforeAnchor+1]

	tamperAuditRow(t, pool, target.ID)

	result, err := audits.VerifyFromLatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("verify from latest anchor: %v", err)
	}
	if result.Valid {
		t.Fatal("valid = true after tampering a tail row, want false")
	}
	if result.FirstBreakID != target.ID {
		t.Errorf("first break id = %q, want the tampered tail row %q", result.FirstBreakID, target.ID)
	}
	// Checked counts only the tail this call actually walked: the tampered
	// row is the 2nd row past the anchor, so Checked must be 2, not
	// beforeAnchor+2 (which would mean it re-walked the anchored prefix too).
	if result.Checked != 2 {
		t.Errorf("checked = %d, want 2 (only the tail past the anchor, up to and including the break)", result.Checked)
	}
}

// TestAuditService_VerifyFromLatestAnchor_SignedAnchorDetectsForgery proves the
// anchor signature (audit remediation) makes a DB-privileged rewrite of the
// audit_anchors row detectable: with signing on, a valid signed anchor still
// bounds verification to the tail, but forging the stored anchor row_hash
// (which an attacker with DB access could do) invalidates the signature they
// cannot recompute, so VerifyFromLatestAnchor reports the chain as not
// verifiable instead of trusting the forged checkpoint.
func TestAuditService_VerifyFromLatestAnchor_SignedAnchorDetectsForgery(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	ctx := context.Background()
	key := []byte("test-anchor-signing-key")
	audits := ledger.NewAuditService(repo, ledger.WithAnchorSigningKey(key))
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "signed anchor forgery test tenant"); err != nil {
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

	postAndDrainChain(t, pool, repo, tenant, cash.ID, revenue.ID, 4)

	// Anchor WITH the signing key, then extend the chain.
	anchorJob := audit.NewAnchorJob(pool, discardLogger(), time.Hour, audit.WithAnchorSigningKey(key))
	if _, err := anchorJob.AnchorOnce(ctx); err != nil {
		t.Fatalf("anchor once: %v", err)
	}
	txns := ledger.NewTransactionService(repo, discardLogger(), nil)
	for i := 0; i < 3; i++ {
		if _, err := txns.Post(ctx, tenant, mkTxn(t, cash.ID, revenue.ID), nil); err != nil {
			t.Fatalf("post tail %d: %v", i, err)
		}
	}
	drainChainer(t, pool, tenant)

	// A genuine signed anchor still verifies, bounded to the tail.
	ok, err := audits.VerifyFromLatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("verify from signed anchor: %v", err)
	}
	if !ok.Valid {
		t.Fatalf("verify from valid signed anchor: Valid=false, want true")
	}

	// Simulate a DB-privileged attacker forging the anchor's stored row_hash.
	tid, _ := uuid.Parse(tenant)
	if _, err := pool.Exec(ctx, "UPDATE audit_anchors SET row_hash = $1 WHERE tenant_id = $2", "forgeddeadbeef", tid); err != nil {
		t.Fatalf("forge anchor row_hash: %v", err)
	}

	forged, err := audits.VerifyFromLatestAnchor(ctx, tenant)
	if err != nil {
		t.Fatalf("verify from forged anchor: %v", err)
	}
	if forged.Valid {
		t.Error("verify from a forged anchor: Valid=true, want false (signature no longer matches the forged row_hash)")
	}
	if forged.Checked != 0 {
		t.Errorf("verify from a forged anchor: Checked=%d, want 0 (stopped at the signature gate, never walked the tail)", forged.Checked)
	}
}
