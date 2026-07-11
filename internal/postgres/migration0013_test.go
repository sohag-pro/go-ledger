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

// TestMigration0013_FingerprintSchemeDefaultsToV1 runs migration 0013 in
// isolation (Task 2.3, audit A1.6), the same up/down/up pattern
// TestMigration0011 and TestMigration0012 use: it migrates to 0012, inserts
// an idempotency_keys row the way every row before this migration looks (no
// fingerprint_scheme column exists yet), migrates forward to 0013, and checks
// the pre-existing row picked up 'v1' (the scheme name this codebase uses
// for every fingerprint computed before this column existed). It then
// inserts a row with an explicit non-default scheme to prove the column
// accepts arbitrary scheme names for a future scheme bump, reverses (down to
// 0012, dropping the column), and re-applies (up to 0013 again), proving the
// migration is cleanly reversible.
func TestMigration0013_FingerprintSchemeDefaultsToV1(t *testing.T) {
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

	// Migrate to just before the scheme column: 0012.
	if err := goose.UpTo(sqlDB, "migrations", 12); err != nil {
		t.Fatalf("migrate to 0012: %v", err)
	}

	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0013 test tenant')`, tenant)

	txnID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`,
		txnID, tenant)

	// A pre-existing row, written the way every row before this migration
	// looks: no fingerprint_scheme column exists at this point.
	mustExecDB(t, sqlDB,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id)
		 VALUES ($1, 'pre-2.3 key', 'fp-pre-2.3', $2)`,
		tenant, txnID)

	// Migrate forward through 0013: the pre-existing row must pick up 'v1'.
	if err := goose.UpTo(sqlDB, "migrations", 13); err != nil {
		t.Fatalf("migrate to 0013: %v", err)
	}
	assertFingerprintScheme(t, sqlDB, tenant, "pre-2.3 key", "v1")

	// A new row can carry a different scheme name, proving the column is a
	// free-form tag ready for a future scheme bump, not hardcoded to 'v1'.
	mustExecDB(t, sqlDB,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, fingerprint_scheme, transaction_id)
		 VALUES ($1, 'v2-key', 'fp-v2', 'v2', $2)`,
		tenant, txnID)
	assertFingerprintScheme(t, sqlDB, tenant, "v2-key", "v2")

	// Down: the column must go away cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 12); err != nil {
		t.Fatalf("migrate down to 0012: %v", err)
	}
	var columnExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'idempotency_keys' AND column_name = 'fingerprint_scheme')`,
	).Scan(&columnExists); err != nil {
		t.Fatalf("check column fingerprint_scheme exists: %v", err)
	}
	if columnExists {
		t.Error("idempotency_keys.fingerprint_scheme still exists after migrating down to 0012")
	}

	// Up again: must re-apply cleanly, and the default must still work for a
	// row written the pre-2.3 way.
	if err := goose.UpTo(sqlDB, "migrations", 13); err != nil {
		t.Fatalf("migrate up to 0013 again: %v", err)
	}
	mustExecDB(t, sqlDB,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id)
		 VALUES ($1, 'post-redo key', 'fp-post-redo', $2)`,
		tenant, txnID)
	assertFingerprintScheme(t, sqlDB, tenant, "post-redo key", "v1")
}

func assertFingerprintScheme(t *testing.T, db *sql.DB, tenant, key, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(
		`SELECT fingerprint_scheme FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenant, key,
	).Scan(&got); err != nil {
		t.Fatalf("get fingerprint_scheme for key %s: %v", key, err)
	}
	if got != want {
		t.Errorf("fingerprint_scheme for key %s = %q, want %q", key, got, want)
	}
}
