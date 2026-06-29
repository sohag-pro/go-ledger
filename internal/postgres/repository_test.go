package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newTestPool starts a throwaway Postgres container, runs the migrations against
// it, and returns a connection pool. The test is skipped (not failed) when no
// Docker daemon is reachable, so `make test` stays green on machines without
// Docker; CI runs Docker and exercises the real path.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping integration test: cannot start postgres container (is Docker running?): %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// Run migrations over a database/sql handle (goose uses database/sql).
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close sql db: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func money(t *testing.T, amount int64, currency string) domain.Money {
	t.Helper()
	m, err := domain.NewMoney(amount, domain.Currency(currency))
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	return m
}

// TestHappyPath is the Week 3 definition of done: create two accounts, post a
// balanced two-leg transaction, and read the derived balances back from a real
// Postgres.
func TestHappyPath(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, cash); err != nil {
		t.Fatalf("create cash account: %v", err)
	}
	if cash.ID == "" {
		t.Fatal("expected generated account id, got empty")
	}

	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, revenue); err != nil {
		t.Fatalf("create revenue account: %v", err)
	}

	// Debit cash 100.00, credit revenue 100.00. Postings sum to zero.
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: cash.ID, Amount: money(t, 10000, "USD")},
		{AccountID: revenue.ID, Amount: money(t, -10000, "USD")},
	}}
	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	if txn.ID == "" {
		t.Fatal("expected generated transaction id, got empty")
	}

	cashBal, err := repo.Balance(ctx, tenant, cash.ID)
	if err != nil {
		t.Fatalf("cash balance: %v", err)
	}
	if cashBal.Amount() != 10000 {
		t.Errorf("cash balance = %d, want 10000", cashBal.Amount())
	}

	revBal, err := repo.Balance(ctx, tenant, revenue.ID)
	if err != nil {
		t.Fatalf("revenue balance: %v", err)
	}
	if revBal.Amount() != -10000 {
		t.Errorf("revenue balance = %d, want -10000", revBal.Amount())
	}

	// The ledger nets to zero: the defining double-entry property, end to end.
	if cashBal.Amount()+revBal.Amount() != 0 {
		t.Errorf("ledger does not net to zero: cash=%d revenue=%d", cashBal.Amount(), revBal.Amount())
	}

	// The transaction round-trips with both postings intact.
	got, err := repo.GetTransaction(ctx, tenant, txn.ID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if len(got.Postings) != 2 {
		t.Fatalf("got %d postings, want 2", len(got.Postings))
	}
	if err := got.Validate(); err != nil {
		t.Errorf("round-tripped transaction does not validate: %v", err)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetAccount(context.Background(), uuid.NewString(), uuid.NewString())
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("got %v, want ErrAccountNotFound", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	owner := uuid.NewString()
	acct := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	if err := repo.CreateAccount(ctx, owner, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// A different tenant must not see the account, even with the right id.
	other := uuid.NewString()
	_, err := repo.GetAccount(ctx, other, acct.ID)
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("cross-tenant read: got %v, want ErrAccountNotFound", err)
	}
}
