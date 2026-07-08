package ledger_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
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
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// dbMaxConns caps the test pool. This is the real ceiling on how many posting
// transactions hit Postgres concurrently, regardless of goroutine count.
const dbMaxConns = 25

// One Postgres container is shared across the package, started once in TestMain.
// Tests scope data by unique tenant ids, so a single container suffices and CI is
// not overwhelmed by one container per test.
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
		// Wait on the readiness log: Postgres opens 5432 during initdb then restarts
		// it, so a port-only wait races real readiness. The log appears twice, hence
		// WithOccurrence(2).
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
	if err := migrate(dsn); err != nil {
		poolErr = err
		return m.Run()
	}
	pool, err := postgres.NewPool(ctx, dsn, dbMaxConns)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	defer pool.Close()
	sharedPool = pool
	return m.Run()
}

func migrate(dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, "migrations")
}

// newTestPool returns the shared pool, skipping when no container was available.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return sharedPool
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPostConcurrentStress is the Week 4 definition of done: many goroutines post
// balanced transactions against a shared pool of accounts, and the ledger stays
// correct under SERIALIZABLE contention. Correctness check: the sum of every
// account balance is exactly zero (money only moved, never appeared or vanished).
//
// Load is spread across multiple tenants rather than hammering one: since
// ADR-012, every posted transaction also extends its tenant's tamper-evident
// audit hash chain (read the tenant's latest row_hash, then insert the next
// row, in the same SERIALIZABLE transaction as the posting). That is a real,
// documented per-tenant serialization point (ADR-012, "Negative
// consequences"), and no amount of retrying inside one tenant's chain changes
// that a hash chain is inherently a sequential structure: a transaction whose
// snapshot predates a sibling's commit cannot "see" that commit mid-flight
// under PostgreSQL SERIALIZABLE, so it aborts and must retry with a fresh
// snapshot regardless of any locking scheme. A single tenant absorbing all
// goroutines here would mean every one of them contends on that one tenant's
// chain tail, which is not how real traffic looks (tenants are independent
// API-key holders) and is exactly the "very high-throughput single tenant"
// case the ADR calls out as a real felt cost, not the normal per-tenant write
// rate the design targets. Partitioning goroutines and accounts across
// several tenants keeps the concurrency and account-contention this test was
// written to exercise, while keeping the per-tenant chain contention at a
// realistic level.
func TestPostConcurrentStress(t *testing.T) {
	const (
		tenantCount       = 10
		accountsPerTenant = 10
		goroutines        = 100
		totalPosts        = 10_000
	)

	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenants := make([]string, tenantCount)
	idsByTenant := make([][]string, tenantCount)
	for tn := range tenants {
		tenants[tn] = uuid.NewString()
		ids := make([]string, accountsPerTenant)
		for i := range ids {
			a := &domain.Account{Name: "acct", Type: domain.Asset, Currency: "USD"}
			if err := repo.CreateAccount(ctx, tenants[tn], a); err != nil {
				t.Fatalf("create account %d for tenant %d: %v", i, tn, err)
			}
			ids[i] = a.ID
		}
		idsByTenant[tn] = ids
	}

	perG := totalPosts / goroutines
	var (
		failures atomic.Int64
		wg       sync.WaitGroup
		latMu    sync.Mutex
		lats     = make([]time.Duration, 0, totalPosts)
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			// Every goroutine sticks to one tenant for its whole run, mirroring
			// how a real API key is scoped to a single tenant.
			tenant := tenants[seed%tenantCount]
			ids := idsByTenant[seed%tenantCount]
			accounts := len(ids)
			rng := rand.New(rand.NewSource(int64(seed) + 1)) //nolint:gosec // test data, not crypto
			local := make([]time.Duration, 0, perG)
			for p := 0; p < perG; p++ {
				from := rng.Intn(accounts)
				to := rng.Intn(accounts - 1)
				if to >= from { // pick a distinct second account
					to++
				}
				amt := rng.Int63n(1_000_000) + 1
				debit, _ := domain.NewMoney(amt, "USD")
				credit, _ := domain.NewMoney(-amt, "USD")
				txn := &domain.Transaction{Postings: []domain.Posting{
					{AccountID: ids[from], Amount: debit},
					{AccountID: ids[to], Amount: credit},
				}}
				start := time.Now()
				if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
					failures.Add(1)
					t.Errorf("post failed: %v", err)
					continue
				}
				local = append(local, time.Since(start))
			}
			latMu.Lock()
			lats = append(lats, local...)
			latMu.Unlock()
		}(g)
	}
	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Fatalf("%d posts failed; expected zero", f)
	}

	// The core invariant, checked end to end: across every account of every
	// tenant, the signed balances sum to exactly zero. A single unbalanced or
	// lost posting breaks it. Accounts never move money across tenants (the
	// composite foreign keys make it impossible), so one combined sum over
	// every tenant's accounts is an equally valid check as summing each
	// tenant separately.
	var total int64
	for tn, ids := range idsByTenant {
		for _, id := range ids {
			bal, err := repo.Balance(ctx, tenants[tn], id)
			if err != nil {
				t.Fatalf("balance %s (tenant %d): %v", id, tn, err)
			}
			total += bal.Amount()
		}
	}
	if total != 0 {
		t.Fatalf("ledger does not net to zero: sum of balances = %d", total)
	}

	p50, p99 := percentile(lats, 0.50), percentile(lats, 0.99)
	t.Logf("posted %d transactions across %d tenants x %d accounts via %d goroutines (DB concurrency capped at MaxConns=%d)",
		len(lats), tenantCount, accountsPerTenant, goroutines, dbMaxConns)
	t.Logf("latency baselines: p50=%s p99=%s", p50, p99)
}

// percentile returns the q-quantile (0..1) of ds. Returns 0 for an empty slice.
func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(q * float64(len(sorted)-1))
	return sorted[idx]
}

// TestUnbalancedRejectedByTrigger proves the DB-level guarantee: even bypassing
// the domain and service entirely and inserting raw rows, the deferred constraint
// trigger rejects an unbalanced transaction at COMMIT.
func TestUnbalancedRejectedByTrigger(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	acct := uuid.New()
	txn := uuid.New()
	posting := uuid.New()

	// Seed an account so the posting's foreign key holds.
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1,$2,'a','asset','USD')`,
		acct, tenant); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed; harmless on error path

	if _, err := tx.Exec(ctx,
		`INSERT INTO transactions (id, tenant_id, currency) VALUES ($1,$2,'USD')`,
		txn, tenant); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	// A single non-zero posting: the transaction cannot possibly balance.
	if _, err := tx.Exec(ctx,
		`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount) VALUES ($1,$2,$3,$4,$5)`,
		posting, tenant, txn, acct, int64(100)); err != nil {
		t.Fatalf("insert posting: %v", err)
	}

	// The trigger is deferred, so the violation surfaces here, at COMMIT.
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("expected commit to fail on unbalanced transaction, got nil")
	}
}

// TestPostRejectsUnbalanced confirms the service fails an unbalanced transaction
// fast, before any database round-trip. A nil repository is safe because Validate
// returns before the repo is touched.
func TestPostRejectsUnbalanced(t *testing.T) {
	svc := ledger.NewTransactionService(nil, discardLogger(), nil)
	debit, _ := domain.NewMoney(100, "USD")
	credit, _ := domain.NewMoney(-50, "USD") // does not offset the debit
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: uuid.NewString(), Amount: debit},
		{AccountID: uuid.NewString(), Amount: credit},
	}}
	if _, err := svc.Post(context.Background(), uuid.NewString(), txn, nil); err == nil {
		t.Fatal("expected unbalanced transaction to be rejected, got nil")
	}
}
