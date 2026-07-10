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

// TestMigration0022_AccountStatusMinBalance runs migration 0022 in isolation
// (Task 5.5, audit A1.5), the same up/down/up pattern the other
// single-migration tests in this package use: it migrates to 0021, inserts
// an account row the way every pre-0022 row looks (no status or min_balance
// columns at all), migrates forward through 0022, proves the pre-existing
// account was backfilled to status='active' and min_balance NULL (every
// account created before this migration keeps posting exactly as before),
// proves the status CHECK constraint rejects an unrecognized value, proves
// min_balance accepts a negative value (a legitimate overdraft allowance,
// not an error), reverses (down to 0021, dropping both columns), and
// re-applies (up to 0022 again), proving the migration is cleanly
// reversible.
func TestMigration0022_AccountStatusMinBalance(t *testing.T) {
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

	// Migrate to just before the new columns: 0021.
	if err := goose.UpTo(sqlDB, "migrations", 21); err != nil {
		t.Fatalf("migrate to 0021: %v", err)
	}

	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0022 test tenant')`, tenant)
	acctID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1, $2, 'Cash', 'asset', 'USD')`,
		acctID, tenant)

	// Migrate forward through 0022: the pre-existing account must be
	// backfilled to status='active', min_balance NULL.
	if err := goose.UpTo(sqlDB, "migrations", 22); err != nil {
		t.Fatalf("migrate to 0022: %v", err)
	}

	var status string
	var minBalance sql.NullInt64
	if err := sqlDB.QueryRow(
		`SELECT status, min_balance FROM accounts WHERE id = $1`, acctID,
	).Scan(&status, &minBalance); err != nil {
		t.Fatalf("read back backfilled account: %v", err)
	}
	if status != "active" {
		t.Errorf("backfilled status = %q, want %q", status, "active")
	}
	if minBalance.Valid {
		t.Errorf("backfilled min_balance = %v, want NULL", minBalance)
	}

	// The status CHECK constraint rejects an unrecognized value.
	if _, err := sqlDB.Exec(`UPDATE accounts SET status = 'bogus' WHERE id = $1`, acctID); err == nil {
		t.Error("expected a CHECK violation setting status to an unrecognized value, got nil")
	}

	// A negative min_balance (a legitimate overdraft allowance, migration
	// 0022's own doc comment) is accepted, not rejected.
	acctID2 := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO accounts (id, tenant_id, name, type, currency, min_balance) VALUES ($1, $2, 'Checking', 'asset', 'USD', -50000)`,
		acctID2, tenant)
	var minBalance2 sql.NullInt64
	if err := sqlDB.QueryRow(`SELECT min_balance FROM accounts WHERE id = $1`, acctID2).Scan(&minBalance2); err != nil {
		t.Fatalf("read back negative min_balance: %v", err)
	}
	if !minBalance2.Valid || minBalance2.Int64 != -50000 {
		t.Errorf("min_balance = %v, want -50000", minBalance2)
	}

	// Down: both columns must be reversed cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 21); err != nil {
		t.Fatalf("migrate down to 0021: %v", err)
	}
	for _, col := range []string{"status", "min_balance"} {
		var columnExists bool
		if err := sqlDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'accounts' AND column_name = $1)`,
			col,
		).Scan(&columnExists); err != nil {
			t.Fatalf("check column %s exists: %v", col, err)
		}
		if columnExists {
			t.Errorf("accounts.%s still exists after migrating down to 0021", col)
		}
	}

	// Up again: must re-apply cleanly, backfilling the same surviving
	// pre-existing rows the same way.
	if err := goose.UpTo(sqlDB, "migrations", 22); err != nil {
		t.Fatalf("migrate up to 0022 again: %v", err)
	}
	var statusAgain string
	if err := sqlDB.QueryRow(`SELECT status FROM accounts WHERE id = $1`, acctID).Scan(&statusAgain); err != nil {
		t.Fatalf("read back re-backfilled account: %v", err)
	}
	if statusAgain != "active" {
		t.Errorf("re-backfilled status = %q, want %q", statusAgain, "active")
	}
}
