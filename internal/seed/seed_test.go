package seed_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/seed"
)

var (
	sharedPool *pgxpool.Pool
	poolErr    error
)

func TestMain(m *testing.M) {
	os.Exit(runWithContainer(m))
}

func runWithContainer(m *testing.M) int {
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
		poolErr = fmt.Errorf("cannot start postgres container (is Docker running?): %w", err)
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		poolErr = err
		return m.Run()
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		poolErr = err
		return m.Run()
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		poolErr = err
		return m.Run()
	}
	_ = sqlDB.Close()

	pool, err := postgres.NewPool(ctx, dsn, 10)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	defer pool.Close()
	sharedPool = pool
	return m.Run()
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return sharedPool
}

func countTxns(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM transactions WHERE tenant_id=$1", tenant).Scan(&n); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	return n
}

func TestSeed(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.New()
	now := time.Now()

	if err := seed.Seed(ctx, pool, tenant.String(), now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Four accounts.
	accts, err := repo.ListAccounts(ctx, tenant.String(), 100)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accts) != 4 {
		t.Fatalf("got %d accounts, want 4", len(accts))
	}

	// The ledger nets to zero across every account: the core invariant holds even
	// for backdated, raw-inserted demo data (the triggers validated each leg).
	var total int64
	for _, a := range accts {
		bal, err := repo.Balance(ctx, tenant.String(), a.ID)
		if err != nil {
			t.Fatalf("balance %s: %v", a.Name, err)
		}
		total += bal.Amount()
	}
	if total != 0 {
		t.Errorf("ledger does not net to zero: sum of balances = %d", total)
	}

	// Transactions are backdated: the oldest posting reaches well into the past.
	var minAt time.Time
	if err := pool.QueryRow(ctx,
		"SELECT min(created_at) FROM postings WHERE tenant_id=$1", tenant).Scan(&minAt); err != nil {
		t.Fatalf("min created_at: %v", err)
	}
	if !minAt.Before(now.AddDate(0, 0, -60)) {
		t.Errorf("oldest posting is %v, expected backdated more than 60 days", minAt)
	}

	// Re-seeding resets rather than appends: the transaction count is stable.
	before := countTxns(t, pool, tenant)
	if err := seed.Seed(ctx, pool, tenant.String(), now); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if after := countTxns(t, pool, tenant); after != before {
		t.Errorf("re-seed changed transaction count: %d then %d (expected reset, not append)", before, after)
	}
}
