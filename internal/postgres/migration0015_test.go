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

// TestMigration0015_AuditOutbox runs migration 0015 (ADR-017) in isolation,
// the same up/down/up pattern TestMigration0014_PerTenantFXRates uses: it
// migrates to 0014, sets up a tenant and a transaction row (audit_outbox's
// foreign key target), migrates forward through 0015, inserts a real outbox
// row and checks its defaults (occurred_at, txid, created_at all populated;
// processed_at NULL), proves the transaction_id foreign key is enforced,
// reverses (down to 0014, dropping the table and its indexes, and restoring
// migration 0009's original audit_log index), and re-applies (up to 0015
// again), proving the migration is cleanly reversible.
func TestMigration0015_AuditOutbox(t *testing.T) {
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

	// Migrate to just before the outbox: 0014.
	if err := goose.UpTo(sqlDB, "migrations", 14); err != nil {
		t.Fatalf("migrate to 0014: %v", err)
	}

	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0015 test tenant')`, tenant)
	txnID := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnID, tenant)

	// Migrate forward through 0015: audit_outbox must exist with the right
	// defaults and constraints.
	if err := goose.UpTo(sqlDB, "migrations", 15); err != nil {
		t.Fatalf("migrate to 0015: %v", err)
	}

	if _, err := sqlDB.Exec(
		`INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, 'transaction.created', $2, $3, '{}'::jsonb)`,
		tenant, txnID, tenant); err != nil {
		t.Fatalf("insert audit_outbox row: %v", err)
	}

	var occurredAt, createdAt time.Time
	var txid int64
	var processedAt sql.NullTime
	if err := sqlDB.QueryRow(
		`SELECT occurred_at, txid, created_at, processed_at FROM audit_outbox WHERE transaction_id = $1`,
		txnID,
	).Scan(&occurredAt, &txid, &createdAt, &processedAt); err != nil {
		t.Fatalf("read back audit_outbox row: %v", err)
	}
	if occurredAt.IsZero() {
		t.Error("occurred_at defaulted to zero, want the database server's now()")
	}
	if txid <= 0 {
		t.Errorf("txid = %d, want a positive transaction id (pg_current_xact_id cast)", txid)
	}
	if createdAt.IsZero() {
		t.Error("created_at defaulted to zero, want the database server's now()")
	}
	if processedAt.Valid {
		t.Errorf("processed_at = %v, want NULL until the chainer processes this row", processedAt.Time)
	}

	// The foreign key must be enforced: a well-formed but nonexistent
	// transaction id is rejected, the same integrity audit_log already has.
	if _, err := sqlDB.Exec(
		`INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, 'transaction.created', $2, $3, '{}'::jsonb)`,
		tenant, uuid.NewString(), tenant,
	); err == nil {
		t.Error("expected a foreign-key violation inserting an outbox row for a nonexistent transaction, got nil")
	}

	// Down: the table (and its indexes, and audit_log's swapped index) must
	// be reversed cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 14); err != nil {
		t.Fatalf("migrate down to 0014: %v", err)
	}
	var tableExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'audit_outbox')`,
	).Scan(&tableExists); err != nil {
		t.Fatalf("check audit_outbox exists: %v", err)
	}
	if tableExists {
		t.Error("audit_outbox still exists after migrating down to 0014")
	}
	var oldIndexExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'audit_log_tenant_created_idx')`,
	).Scan(&oldIndexExists); err != nil {
		t.Fatalf("check audit_log_tenant_created_idx exists: %v", err)
	}
	if !oldIndexExists {
		t.Error("audit_log_tenant_created_idx was not restored after migrating down to 0014")
	}

	// Up again: must re-apply cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 15); err != nil {
		t.Fatalf("migrate up to 0015 again: %v", err)
	}
	var tableExistsAgain bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'audit_outbox')`,
	).Scan(&tableExistsAgain); err != nil {
		t.Fatalf("check audit_outbox exists after re-migrating: %v", err)
	}
	if !tableExistsAgain {
		t.Error("audit_outbox does not exist after migrating up to 0015 again")
	}
}
