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

// TestMigration0017_Reversal runs migration 0017 in isolation (Task 4.2, audit
// A1.2), the same up/down/up pattern the other single-migration tests in this
// package use: it migrates to 0016, inserts an original transaction the way
// every pre-0017 row looks (no reverses_transaction_id column yet), migrates
// forward through 0017, links a second transaction to the first as its
// reversal, proves the tenant-scoped foreign key rejects a nonexistent
// original, proves the unique partial index rejects a SECOND reversal of the
// same original, reverses (down to 0016, dropping the column, its foreign key,
// and the unique index), and re-applies (up to 0017 again), proving the
// migration is cleanly reversible.
func TestMigration0017_Reversal(t *testing.T) {
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

	// Migrate to just before the reversal link: 0016.
	if err := goose.UpTo(sqlDB, "migrations", 16); err != nil {
		t.Fatalf("migrate to 0016: %v", err)
	}

	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0017 test tenant')`, tenant)
	original := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, original, tenant)

	// Migrate forward through 0017: the column, its foreign key, and its
	// unique partial index must all be in place.
	if err := goose.UpTo(sqlDB, "migrations", 17); err != nil {
		t.Fatalf("migrate to 0017: %v", err)
	}

	reversal := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO transactions (id, tenant_id, reverses_transaction_id) VALUES ($1, $2, $3)`,
		reversal, tenant, original)

	var linked uuid.UUID
	if err := sqlDB.QueryRow(
		`SELECT reverses_transaction_id FROM transactions WHERE id = $1`, reversal,
	).Scan(&linked); err != nil {
		t.Fatalf("read back reverses_transaction_id: %v", err)
	}
	if linked.String() != original {
		t.Errorf("reverses_transaction_id = %s, want %s", linked, original)
	}

	// The tenant-scoped foreign key must reject a reversal pointing at a
	// transaction id that does not exist for this tenant.
	if _, err := sqlDB.Exec(
		`INSERT INTO transactions (id, tenant_id, reverses_transaction_id) VALUES ($1, $2, $3)`,
		uuid.NewString(), tenant, uuid.NewString(),
	); err == nil {
		t.Error("expected a foreign-key violation reversing a nonexistent transaction, got nil")
	}

	// The unique partial index must reject a SECOND reversal of the same
	// original: this is the database-level guard ReverseTransaction's
	// idempotency and concurrent-double-reverse handling depends on.
	if _, err := sqlDB.Exec(
		`INSERT INTO transactions (id, tenant_id, reverses_transaction_id) VALUES ($1, $2, $3)`,
		uuid.NewString(), tenant, original,
	); err == nil {
		t.Error("expected a unique violation on a second reversal of the same original, got nil")
	}

	// Down: the column (and its foreign key and index) must be reversed cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 16); err != nil {
		t.Fatalf("migrate down to 0016: %v", err)
	}
	var columnExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'transactions' AND column_name = 'reverses_transaction_id')`,
	).Scan(&columnExists); err != nil {
		t.Fatalf("check column transactions.reverses_transaction_id exists: %v", err)
	}
	if columnExists {
		t.Error("transactions.reverses_transaction_id still exists after migrating down to 0016")
	}

	// Up again: must re-apply cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 17); err != nil {
		t.Fatalf("migrate up to 0017 again: %v", err)
	}
	var columnExistsAgain bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'transactions' AND column_name = 'reverses_transaction_id')`,
	).Scan(&columnExistsAgain); err != nil {
		t.Fatalf("check column exists after re-migrating: %v", err)
	}
	if !columnExistsAgain {
		t.Error("transactions.reverses_transaction_id does not exist after migrating up to 0017 again")
	}
}
