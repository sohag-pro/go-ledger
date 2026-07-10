package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestMigration0023_CompositeTenantFKs runs migration 0023 (Task 5.4a, audit
// A4.4) in isolation, the same up/down/up pattern the other single-migration
// tests in this package use: it migrates to 0022, sets up two tenants and a
// transaction under each, migrates forward through 0023, proves that for
// each of idempotency_keys, audit_log, and audit_outbox a row whose
// transaction_id belongs to a transaction under a DIFFERENT tenant than the
// row's own tenant_id is now rejected (the single-column FK migrations 0006
// and 0015 originally added never checked tenant_id at all, so this exact
// row would have been accepted before this migration), proves a correctly
// tenant-matched row still inserts for all three tables, reverses (down to
// 0022, restoring the single-column FK, under which the same cross-tenant
// row is accepted again), and re-applies (up to 0023 again), proving the
// migration is cleanly reversible.
func TestMigration0023_CompositeTenantFKs(t *testing.T) {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping integration test: cannot start postgres container (is Docker running?): %v", err)
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Migrate to just before the composite FKs: 0022.
	if err := goose.UpTo(sqlDB, "migrations", 22); err != nil {
		t.Fatalf("migrate to 0022: %v", err)
	}

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0023 tenant A')`, tenantA)
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0023 tenant B')`, tenantB)

	txnA := uuid.NewString()
	txnB := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnA, tenantA)
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnB, tenantB)

	// Before this migration, the FK is single-column (transaction_id ->
	// transactions.id only), so a row claiming tenant_id = B but pointing at
	// a transaction that actually belongs to tenant A is accepted: the FK
	// has no way to notice the mismatch.
	mustExecDB(t, sqlDB,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id, expires_at)
		 VALUES ($1, 'pre-migration-cross-tenant-key', 'fp', $2, now() + interval '24 hours')`,
		tenantB, txnA)
	mustExecDB(t, sqlDB,
		`DELETE FROM idempotency_keys WHERE idempotency_key = 'pre-migration-cross-tenant-key'`)

	// Migrate forward through 0023: the composite FK must now be enforced.
	if err := goose.UpTo(sqlDB, "migrations", 23); err != nil {
		t.Fatalf("migrate to 0023: %v", err)
	}

	// idempotency_keys: cross-tenant row (tenant_id = B, transaction_id
	// belongs to A) is rejected.
	if _, err := sqlDB.Exec(
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id, expires_at)
		 VALUES ($1, 'cross-tenant-key', 'fp', $2, now() + interval '24 hours')`,
		tenantB, txnA,
	); err == nil {
		t.Error("idempotency_keys: expected a foreign-key violation for a cross-tenant transaction_id, got nil")
	}
	// Correctly tenant-matched row still inserts.
	if _, err := sqlDB.Exec(
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id, expires_at)
		 VALUES ($1, 'matched-key', 'fp', $2, now() + interval '24 hours')`,
		tenantA, txnA,
	); err != nil {
		t.Errorf("idempotency_keys: matched tenant_id/transaction_id row rejected: %v", err)
	}

	// audit_log: cross-tenant row is rejected.
	if _, err := sqlDB.Exec(
		`INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, $2, 'transaction.created', $3, $4, '{}')`,
		uuid.NewString(), tenantB, txnA, "tester",
	); err == nil {
		t.Error("audit_log: expected a foreign-key violation for a cross-tenant transaction_id, got nil")
	}
	// Correctly tenant-matched row still inserts.
	if _, err := sqlDB.Exec(
		`INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, $2, 'transaction.created', $3, $4, '{}')`,
		uuid.NewString(), tenantA, txnA, "tester",
	); err != nil {
		t.Errorf("audit_log: matched tenant_id/transaction_id row rejected: %v", err)
	}

	// audit_outbox: cross-tenant row is rejected.
	if _, err := sqlDB.Exec(
		`INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, 'transaction.created', $2, $3, '{}')`,
		tenantB, txnA, "tester",
	); err == nil {
		t.Error("audit_outbox: expected a foreign-key violation for a cross-tenant transaction_id, got nil")
	}
	// Correctly tenant-matched row still inserts.
	if _, err := sqlDB.Exec(
		`INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, 'transaction.created', $2, $3, '{}')`,
		tenantA, txnA, "tester",
	); err != nil {
		t.Errorf("audit_outbox: matched tenant_id/transaction_id row rejected: %v", err)
	}

	// Down: the single-column FKs must be restored, and under them the same
	// cross-tenant row is accepted again (proving the down migration truly
	// reverses to the pre-0023 behavior, not just to a passing state).
	if err := goose.DownTo(sqlDB, "migrations", 22); err != nil {
		t.Fatalf("migrate down to 0022: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id, expires_at)
		 VALUES ($1, 'post-down-cross-tenant-key', 'fp', $2, now() + interval '24 hours')`,
		tenantB, txnA,
	); err != nil {
		t.Errorf("idempotency_keys: cross-tenant row rejected after down-migrating to 0022, want the old single-column FK to accept it: %v", err)
	}
	// Clean up: this row deliberately violates the composite FK, so it must
	// be removed before migrating back up, the same way a real deployment
	// would have to fix any pre-existing bad data before this migration
	// could apply (the brief's own assumption is that no real row is bad).
	mustExecDB(t, sqlDB, `DELETE FROM idempotency_keys WHERE idempotency_key = 'post-down-cross-tenant-key'`)

	// Up again: the composite FK must re-apply cleanly and re-enforce.
	if err := goose.UpTo(sqlDB, "migrations", 23); err != nil {
		t.Fatalf("migrate up to 0023 again: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id, expires_at)
		 VALUES ($1, 'cross-tenant-key-again', 'fp', $2, now() + interval '24 hours')`,
		tenantB, txnA,
	); err == nil {
		t.Error("idempotency_keys: expected a foreign-key violation for a cross-tenant transaction_id after re-migrating to 0023, got nil")
	}
}
