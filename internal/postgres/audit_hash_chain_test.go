package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// appendAudit writes one audit row for txnID under tenant, inside its own
// RunInTx, mirroring how the ledger service calls AppendAudit alongside
// CreateTransaction. Callers that need the row's hashes should read it back
// via ListAuditForVerify.
func appendAudit(t *testing.T, repo *postgres.Repository, tenant, txnID string) {
	t.Helper()
	ctx := context.Background()
	err := repo.RunInTx(ctx, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAudit(ctx, tenant, domain.AuditEntry{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txnID,
			Actor:         tenant,
			After:         []byte(`{"id":"` + txnID + `"}`),
		})
	})
	if err != nil {
		t.Fatalf("append audit for %s: %v", txnID, err)
	}
}

// TestAuditHashChainBuildsAcrossTransactions posts two transactions for one
// tenant and proves the resulting audit rows form a genuine chain: the first
// row's prev_hash is genesis, the second row's prev_hash equals the first
// row's row_hash, and every row's row_hash recomputes exactly from its own
// stored content and its predecessor's stored hash.
func TestAuditHashChainBuildsAcrossTransactions(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	txn1, _, _ := seedTxn(t, repo, tenant)
	appendAudit(t, repo, tenant, txn1)
	txn2, _, _ := seedTxn(t, repo, tenant)
	appendAudit(t, repo, tenant, txn2)

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	row1, row2 := rows[0], rows[1]

	if row1.PrevHash != domain.AuditGenesisHash {
		t.Errorf("row1 prev_hash = %q, want genesis %q", row1.PrevHash, domain.AuditGenesisHash)
	}
	if want := domain.ComputeAuditRowHash(tenant, row1, domain.AuditGenesisHash); row1.RowHash != want {
		t.Errorf("row1 row_hash = %q, want recomputed %q", row1.RowHash, want)
	}

	if row2.PrevHash != row1.RowHash {
		t.Errorf("row2 prev_hash = %q, want row1's row_hash %q", row2.PrevHash, row1.RowHash)
	}
	if want := domain.ComputeAuditRowHash(tenant, row2, row1.RowHash); row2.RowHash != want {
		t.Errorf("row2 row_hash = %q, want recomputed %q", row2.RowHash, want)
	}
}

// TestAuditHashChainVerifiesWholeWalk proves a longer chain (five
// transactions) is a strict per-tenant sequence: walking it oldest first and
// recomputing each row's hash from its own content plus the previous row's
// recomputed hash reproduces every stored row_hash, exactly the check the
// verify endpoint (Task 9) will perform.
func TestAuditHashChainVerifiesWholeWalk(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const n = 5
	for i := 0; i < n; i++ {
		txnID, _, _ := seedTxn(t, repo, tenant)
		appendAudit(t, repo, tenant, txnID)
	}

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("audit rows = %d, want %d", len(rows), n)
	}

	prev := domain.AuditGenesisHash
	for i, row := range rows {
		if row.PrevHash != prev {
			t.Fatalf("row %d: prev_hash = %q, want %q", i, row.PrevHash, prev)
		}
		recomputed := domain.ComputeAuditRowHash(tenant, row, prev)
		if recomputed != row.RowHash {
			t.Fatalf("row %d: recomputed hash %q != stored row_hash %q", i, recomputed, row.RowHash)
		}
		prev = row.RowHash
	}
}

// TestAuditHashChainDetectsTamper proves the chain is genuinely tamper
// evident: a privileged raw UPDATE that bypasses the immutability trigger (the
// seeder's own escape hatch, SET LOCAL audit.allow_purge = 'on') changes the
// row's content without updating its stored row_hash, so recomputing the hash
// from the tampered content no longer matches what is stored. The trigger
// guards against casual mutation; this chain is what catches a privileged
// rewrite that gets past it.
func TestAuditHashChainDetectsTamper(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	txnID, _, _ := seedTxn(t, repo, tenant)
	appendAudit(t, repo, tenant, txnID)

	before, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify (before tamper): %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(before))
	}
	row := before[0]
	if recomputed := domain.ComputeAuditRowHash(tenant, row, domain.AuditGenesisHash); recomputed != row.RowHash {
		t.Fatalf("row failed to verify before any tampering: recomputed %q != stored %q", recomputed, row.RowHash)
	}

	// Tamper: rewrite `after` directly, bypassing the application entirely.
	// This only succeeds because we deliberately flip the same GUC gate the
	// seeder uses; the application path never does this.
	rowID := uuid.MustParse(row.ID)
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
		[]byte(`{"id":"`+txnID+`","tampered":true}`), rowID,
	); err != nil {
		t.Fatalf("tamper update: %v", err)
	}
	if err := tamperTx.Commit(ctx); err != nil {
		t.Fatalf("commit tamper: %v", err)
	}

	after, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify (after tamper): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("audit rows after tamper = %d, want 1", len(after))
	}
	tampered := after[0]
	// The row_hash on disk is untouched (we only rewrote `after`), so
	// recomputing from the now-tampered content must no longer match it.
	if recomputed := domain.ComputeAuditRowHash(tenant, tampered, tampered.PrevHash); recomputed == tampered.RowHash {
		t.Error("tampering with `after` was not detected: recomputed hash still matched stored row_hash")
	}
}

// TestAuditHashChainDetectsTenantRewrite proves the tenant id is genuinely
// part of what row_hash covers, not just a structural scoping detail: a
// privileged raw UPDATE that rewrites a row's tenant_id (moving it into
// another tenant's chain), bypassing the immutability trigger the same way
// TestAuditHashChainDetectsTamper does, is detectable. Recomputing the hash
// with the row's current (rewritten) tenant id no longer matches the
// row_hash that was stored under the row's original tenant id.
func TestAuditHashChainDetectsTenantRewrite(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()

	txnID, _, _ := seedTxn(t, repo, tenantA)
	appendAudit(t, repo, tenantA, txnID)

	before, err := repo.ListAuditForVerify(ctx, tenantA)
	if err != nil {
		t.Fatalf("list audit for verify (before rewrite): %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(before))
	}
	row := before[0]
	if recomputed := domain.ComputeAuditRowHash(tenantA, row, domain.AuditGenesisHash); recomputed != row.RowHash {
		t.Fatalf("row failed to verify before any rewrite: recomputed %q != stored %q", recomputed, row.RowHash)
	}

	// Rewrite: move the row into tenant B's chain by changing only tenant_id,
	// bypassing the application entirely. This only succeeds because we
	// deliberately flip the same GUC gate the seeder uses; the application
	// path never does this.
	rowID := uuid.MustParse(row.ID)
	tamperTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rewrite tx: %v", err)
	}
	defer tamperTx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if _, err := tamperTx.Exec(ctx, `SET LOCAL audit.allow_purge = 'on'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := tamperTx.Exec(ctx,
		`UPDATE audit_log SET tenant_id = $1 WHERE id = $2`,
		uuid.MustParse(tenantB), rowID,
	); err != nil {
		t.Fatalf("rewrite tenant_id: %v", err)
	}
	if err := tamperTx.Commit(ctx); err != nil {
		t.Fatalf("commit rewrite: %v", err)
	}

	// The row now surfaces under tenant B's chain.
	rewritten, err := repo.ListAuditForVerify(ctx, tenantB)
	if err != nil {
		t.Fatalf("list audit for verify (after rewrite): %v", err)
	}
	if len(rewritten) != 1 {
		t.Fatalf("tenant B audit rows after rewrite = %d, want 1", len(rewritten))
	}
	claimed := rewritten[0]

	// The row_hash on disk is untouched (only tenant_id changed), so
	// recomputing with the tenant the row currently claims (tenant B) must no
	// longer match the row_hash stored under its original tenant (tenant A).
	if recomputed := domain.ComputeAuditRowHash(tenantB, claimed, claimed.PrevHash); recomputed == claimed.RowHash {
		t.Error("tenant_id rewrite was not detected: recomputed hash still matched stored row_hash")
	}
}

// TestAuditHashChainPerTenantIndependent proves each tenant's chain is
// independent: a second tenant's first row starts at genesis regardless of
// how much audit activity another tenant already has, and a tenant's later
// row always chains off its own previous row, never another tenant's.
func TestAuditHashChainPerTenantIndependent(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()

	txnA1, _, _ := seedTxn(t, repo, tenantA)
	appendAudit(t, repo, tenantA, txnA1)

	// Tenant B posts its first row after tenant A already has history; that
	// history must not leak into tenant B's chain.
	txnB1, _, _ := seedTxn(t, repo, tenantB)
	appendAudit(t, repo, tenantB, txnB1)

	rowsA, err := repo.ListAuditForVerify(ctx, tenantA)
	if err != nil {
		t.Fatalf("list audit for tenant A: %v", err)
	}
	rowsB, err := repo.ListAuditForVerify(ctx, tenantB)
	if err != nil {
		t.Fatalf("list audit for tenant B: %v", err)
	}
	if len(rowsA) != 1 || len(rowsB) != 1 {
		t.Fatalf("rowsA = %d, rowsB = %d, want 1 and 1", len(rowsA), len(rowsB))
	}
	if rowsB[0].PrevHash != domain.AuditGenesisHash {
		t.Errorf("tenant B's first row prev_hash = %q, want genesis (unaffected by tenant A's chain)", rowsB[0].PrevHash)
	}
	if rowsA[0].RowHash == rowsB[0].RowHash {
		t.Error("tenant A and tenant B produced the same row_hash for their first row, expected them to differ (different transaction ids)")
	}

	// A second row for tenant A must chain off tenant A's own prior row, not
	// tenant B's, even though tenant B's row was written in between.
	txnA2, _, _ := seedTxn(t, repo, tenantA)
	appendAudit(t, repo, tenantA, txnA2)

	rowsA2, err := repo.ListAuditForVerify(ctx, tenantA)
	if err != nil {
		t.Fatalf("list audit for tenant A (second read): %v", err)
	}
	if len(rowsA2) != 2 {
		t.Fatalf("tenant A audit rows = %d, want 2", len(rowsA2))
	}
	if rowsA2[1].PrevHash != rowsA2[0].RowHash {
		t.Errorf("tenant A's second row prev_hash = %q, want tenant A's first row_hash %q", rowsA2[1].PrevHash, rowsA2[0].RowHash)
	}
	if rowsA2[1].PrevHash == rowsB[0].RowHash {
		t.Error("tenant A's second row chained off tenant B's row_hash")
	}
}
