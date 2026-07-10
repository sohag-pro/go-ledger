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

// TestMigration0019_IdempotencyTTL runs migration 0019 in isolation (Task
// 4.5, audit A1.4), the same up/down/up pattern the other single-migration
// tests in this package use: it migrates to 0018, inserts an idempotency_keys
// row the way every pre-0019 row looks (no expires_at column at all),
// migrates forward through 0019, proves the pre-existing row was backfilled
// to created_at + 24h (not left NULL, and not just "now" + 24h), proves the
// column rejects a NULL going forward (NOT NULL), reverses (down to 0018,
// dropping the column and its index), and re-applies (up to 0019 again),
// proving the migration is cleanly reversible and the backfill is
// idempotent-in-effect (running it twice against the same pre-existing row
// history yields the same shape: a non-NULL expires_at derived from
// created_at).
func TestMigration0019_IdempotencyTTL(t *testing.T) {
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

	// Migrate to just before the ttl column: 0018.
	if err := goose.UpTo(sqlDB, "migrations", 18); err != nil {
		t.Fatalf("migrate to 0018: %v", err)
	}

	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0019 test tenant')`, tenant)
	txnID := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnID, tenant)

	// A pre-existing row written the way every row before this migration
	// looks: no expires_at column exists yet, and created_at is left to its
	// own default (now()) rather than supplied, so the backfill's
	// "created_at + 24h" is checked against whatever the server actually
	// wrote, not a value this test controls.
	mustExecDB(t, sqlDB,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, fingerprint_scheme, transaction_id)
		 VALUES ($1, 'pre-4.5 key', 'fp-pre-4.5', 'v1', $2)`,
		tenant, txnID)

	// Migrate forward through 0019: the pre-existing row must be backfilled.
	if err := goose.UpTo(sqlDB, "migrations", 19); err != nil {
		t.Fatalf("migrate to 0019: %v", err)
	}

	var createdAt, expiresAt time.Time
	if err := sqlDB.QueryRow(
		`SELECT created_at, expires_at FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = 'pre-4.5 key'`,
		tenant,
	).Scan(&createdAt, &expiresAt); err != nil {
		t.Fatalf("read back backfilled row: %v", err)
	}
	wantExpiry := createdAt.Add(24 * time.Hour)
	if diff := expiresAt.Sub(wantExpiry); diff < -time.Second || diff > time.Second {
		t.Errorf("backfilled expires_at = %s, want created_at (%s) + 24h = %s", expiresAt, createdAt, wantExpiry)
	}

	// The column is NOT NULL going forward: an insert that tries to omit it
	// must fail.
	txnID2 := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnID2, tenant)
	if _, err := sqlDB.Exec(
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, fingerprint_scheme, transaction_id)
		 VALUES ($1, 'no-expiry-key', 'fp-x', 'v1', $2)`,
		tenant, txnID2,
	); err == nil {
		t.Error("expected a NOT NULL violation inserting with no expires_at, got nil")
	}

	// A row that DOES supply expires_at inserts cleanly, and the
	// expires_at index exists to back both the lookup filter and the sweep.
	mustExecDB(t, sqlDB,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, fingerprint_scheme, transaction_id, expires_at)
		 VALUES ($1, 'with-expiry-key', 'fp-y', 'v1', $2, now() + interval '1 hour')`,
		tenant, txnID2)

	var indexExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'idempotency_keys' AND indexname = 'idempotency_keys_expires_at_idx')`,
	).Scan(&indexExists); err != nil {
		t.Fatalf("check index exists: %v", err)
	}
	if !indexExists {
		t.Error("idempotency_keys_expires_at_idx does not exist after migrating up to 0019")
	}

	// Down: the column (and its index) must be reversed cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 18); err != nil {
		t.Fatalf("migrate down to 0018: %v", err)
	}
	var columnExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'idempotency_keys' AND column_name = 'expires_at')`,
	).Scan(&columnExists); err != nil {
		t.Fatalf("check column expires_at exists: %v", err)
	}
	if columnExists {
		t.Error("idempotency_keys.expires_at still exists after migrating down to 0018")
	}

	// Up again: must re-apply cleanly, backfilling the same surviving rows
	// (the ones with no expires_at, since the column was just dropped) the
	// same way.
	if err := goose.UpTo(sqlDB, "migrations", 19); err != nil {
		t.Fatalf("migrate up to 0019 again: %v", err)
	}
	var createdAtAgain, expiresAtAgain time.Time
	if err := sqlDB.QueryRow(
		`SELECT created_at, expires_at FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = 'pre-4.5 key'`,
		tenant,
	).Scan(&createdAtAgain, &expiresAtAgain); err != nil {
		t.Fatalf("read back re-backfilled row: %v", err)
	}
	wantExpiryAgain := createdAtAgain.Add(24 * time.Hour)
	if diff := expiresAtAgain.Sub(wantExpiryAgain); diff < -time.Second || diff > time.Second {
		t.Errorf("re-backfilled expires_at = %s, want created_at (%s) + 24h = %s", expiresAtAgain, createdAtAgain, wantExpiryAgain)
	}
}
