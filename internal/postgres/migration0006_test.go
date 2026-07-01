package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditLogIsImmutable(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	acct := uuid.New()
	txn := uuid.New()
	audit := uuid.New()

	// Minimal valid rows to satisfy the FKs. Insert postings-free: the balance
	// trigger is deferred and fires on postings, not on a bare transaction row.
	mustExec(t, pool, `INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1,$2,'A','asset','USD')`, acct, tenant)
	mustExec(t, pool, `INSERT INTO transactions (id, tenant_id, currency) VALUES ($1,$2,'USD')`, txn, tenant)
	// actor gets its own placeholder ($4) rather than reusing $2: Postgres cannot
	// deduce a single type for $2 when it is bound as uuid (tenant_id) and cast to
	// text (actor) in the same statement.
	mustExec(t, pool, `INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, after) VALUES ($1,$2,'transaction.created',$3,$4,'{}'::jsonb)`, audit, tenant, txn, tenant.String())

	// UPDATE is rejected.
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET actor = 'x' WHERE id = $1`, audit); err == nil {
		t.Error("expected UPDATE on audit_log to be rejected, got nil error")
	}
	// DELETE is rejected.
	if _, err := pool.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, audit); err == nil {
		t.Error("expected DELETE on audit_log to be rejected, got nil error")
	}

	// The GUC gate allows a delete (this is the seeder's escape hatch). Do it in
	// one transaction so SET LOCAL applies to the DELETE.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `SET LOCAL audit.allow_purge = 'on'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, audit); err != nil {
		t.Fatalf("gated delete should succeed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
