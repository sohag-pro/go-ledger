package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/metrics"
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
		// Wait on the readiness log, not just the open port: Postgres opens 5432
		// during initdb and then restarts it, so a port-only wait races the real
		// readiness and causes connection resets under parallel container startup
		// (notably in CI). The startup log appears twice (initdb, then the real
		// server), hence WithOccurrence(2).
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
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

	pool, err := postgres.NewPool(ctx, dsn, 10)
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

// TestCurrencyMismatchRejectedByTrigger proves the DB-level guarantee from
// ADR-005: a posting into an account whose currency differs from its
// transaction's currency is rejected, even when inserted as raw rows.
func TestCurrencyMismatchRejectedByTrigger(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	acct := uuid.New()
	txn := uuid.New()
	posting := uuid.New()

	// Account holds EUR; the transaction is in USD.
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1,$2,'a','asset','EUR')`,
		acct, tenant); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO transactions (id, tenant_id, currency) VALUES ($1,$2,'USD')`,
		txn, tenant); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	// The immediate trigger fires on this insert and must reject it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount) VALUES ($1,$2,$3,$4,$5)`,
		posting, tenant, txn, acct, int64(100)); err == nil {
		t.Fatal("expected posting into a EUR account from a USD transaction to be rejected, got nil")
	}
}

func TestListAccounts(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	for _, name := range []string{"Revenue", "Cash"} {
		a := &domain.Account{Name: name, Type: domain.Asset, Currency: "USD"}
		if err := repo.CreateAccount(ctx, tenant, a); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	got, err := repo.ListAccounts(ctx, tenant, 100)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got))
	}
	// Ordered by name: Cash before Revenue.
	if got[0].Name != "Cash" || got[1].Name != "Revenue" {
		t.Errorf("order = %s, %s; want Cash, Revenue", got[0].Name, got[1].Name)
	}
}

// TestAccountStatement exercises the window-function + keyset statement query
// against real Postgres: ordering (newest first), running balance, descriptions,
// and cursor pagination across pages.
func TestAccountStatement(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	other := &domain.Account{Name: "Other", Type: domain.Asset, Currency: "USD"}
	for _, a := range []*domain.Account{cash, other} {
		if err := repo.CreateAccount(ctx, tenant, a); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}

	// Three deposits of 100 into cash. Running balances will be 100, 200, 300.
	for i := 0; i < 3; i++ {
		debit, _ := domain.NewMoney(100, "USD")
		credit, _ := domain.NewMoney(-100, "USD")
		txn := &domain.Transaction{Postings: []domain.Posting{
			{AccountID: cash.ID, Amount: debit, Description: "deposit"},
			{AccountID: other.ID, Amount: credit},
		}}
		if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}

	// First page of 2, newest first: running balances 300 then 200.
	page1, err := repo.Statement(ctx, tenant, cash.ID, "USD", nil, 2)
	if err != nil {
		t.Fatalf("statement page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 has %d entries, want 2", len(page1))
	}
	if page1[0].RunningBalance.Amount() != 300 || page1[1].RunningBalance.Amount() != 200 {
		t.Errorf("page1 running balances = %d,%d want 300,200",
			page1[0].RunningBalance.Amount(), page1[1].RunningBalance.Amount())
	}
	if page1[0].Description != "deposit" {
		t.Errorf("description = %q, want deposit", page1[0].Description)
	}

	// Second page via keyset cursor at the last entry: the remaining one, running 100.
	last := page1[len(page1)-1]
	cursor := &domain.StatementCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	page2, err := repo.Statement(ctx, tenant, cash.ID, "USD", cursor, 2)
	if err != nil {
		t.Fatalf("statement page2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 has %d entries, want 1", len(page2))
	}
	if page2[0].RunningBalance.Amount() != 100 {
		t.Errorf("page2 running balance = %d, want 100", page2[0].RunningBalance.Amount())
	}
}

// serErr returns a synthetic Postgres serialization failure, letting RunInTx's
// retry path be exercised deterministically without manufacturing a real
// read/write conflict.
func serErr() error {
	return &pgconn.PgError{Code: "40001", Message: "synthetic serialization failure"}
}

// TestRunInTxRetriesThenCommits feeds RunInTx one serialization failure followed
// by success, and checks it retried exactly once (counter +1) and committed.
// Not parallel: it asserts on the global retries counter.
func TestRunInTxRetriesThenCommits(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	before := testutil.ToFloat64(metrics.SerializationRetries)
	calls := 0
	err := repo.RunInTx(context.Background(), func(_ context.Context, _ domain.Tx) error {
		calls++
		if calls == 1 {
			return serErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after one retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
	if delta := testutil.ToFloat64(metrics.SerializationRetries) - before; delta != 1 {
		t.Errorf("expected 1 retry recorded, got %v", delta)
	}
}

// TestRunInTxExhaustionReturnsConflict checks that a persistent serialization
// failure ends as a typed, transient domain.ErrConflict, not a raw pg error.
func TestRunInTxExhaustionReturnsConflict(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	err := repo.RunInTx(context.Background(), func(_ context.Context, _ domain.Tx) error {
		return serErr()
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict after exhaustion, got %v", err)
	}
}

// TestRunInTxNonRetryablePropagates checks that an ordinary error is returned
// immediately, without retrying.
func TestRunInTxNonRetryablePropagates(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	sentinel := errors.New("boom")
	calls := 0
	err := repo.RunInTx(context.Background(), func(_ context.Context, _ domain.Tx) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected no retry for non-serialization error, got %d attempts", calls)
	}
}
