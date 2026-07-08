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
	sharedDSN  string
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
	sharedDSN = dsn
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

// newTestPoolWithMaxConns returns a dedicated pool against the same shared
// container, capped at maxConns. Some tests (the no-starvation test below)
// need a small, deliberately scarce pool to make connection contention
// observable; the package-wide sharedPool's dbMaxConns is sized for
// throughput tests instead. The caller's test closes the pool via t.Cleanup.
func newTestPoolWithMaxConns(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	pool, err := postgres.NewPool(context.Background(), sharedDSN, maxConns)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPostConcurrentStress is the Week 4 definition of done: many goroutines post
// balanced transactions against a shared pool of accounts, and the ledger stays
// correct under SERIALIZABLE contention. Correctness check: the sum of every
// account balance is exactly zero (money only moved, never appeared or vanished).
//
// Load is spread across multiple tenants rather than hammering one, mirroring
// how real traffic looks: tenants are independent API-key holders, and this
// test's job is to exercise cross-account contention and the retry loop, not
// to be the definitive single-tenant concurrency test (see
// TestPostConcurrentStressSingleTenant for that: since RunInTx holds a
// per-tenant in-process mutex before opening its transaction, see
// internal/postgres/repository.go, same-tenant posts now serialize one at a
// time and different tenants run fully in parallel, which is what this test
// exercises across its 10 tenants).
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

// TestPostConcurrentStressSingleTenant is the single-tenant counterpart to
// TestPostConcurrentStress, and the regression test for the fix described in
// internal/postgres/repository.go's RunInTx: without any per-tenant
// serialization, 100 fully concurrent posts to one tenant reliably exhausted
// the SERIALIZABLE retry budget. Every post reads the tenant's latest audit
// row_hash and then inserts the next row (ADR-012's hash chain), so
// concurrent same-tenant posts raced on that read; PostgreSQL aborted the
// loser with a serialization failure (SQLSTATE 40001), and at 100-way
// concurrency the 25-attempt retry budget ran out, surfacing as
// domain.ErrConflict (a 503 at the API layer). RunInTx now holds a per-tenant
// in-process mutex before opening any transaction, so these 100 posts
// serialize one at a time instead of racing: this test asserts every one of
// them succeeds, and then walks the tenant's full audit chain
// (AuditService.Verify) to prove the serialized posts produced a genuinely
// unbroken, correctly ordered hash chain, not just "no errors returned."
func TestPostConcurrentStressSingleTenant(t *testing.T) {
	const (
		accounts   = 10
		goroutines = 100
	)

	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	auditSvc := ledger.NewAuditService(repo)
	ctx := context.Background()

	tenant := uuid.NewString()
	ids := make([]string, accounts)
	for i := range ids {
		a := &domain.Account{Name: "acct", Type: domain.Asset, Currency: "USD"}
		if err := repo.CreateAccount(ctx, tenant, a); err != nil {
			t.Fatalf("create account %d: %v", i, err)
		}
		ids[i] = a.ID
	}

	var (
		failures atomic.Int64
		wg       sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed) + 1)) //nolint:gosec // test data, not crypto
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
			if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
				failures.Add(1)
				t.Errorf("post failed: %v", err)
			}
		}(g)
	}
	wg.Wait()

	// The crux of this test: before per-tenant serialization existed, this
	// reliably failed with domain.ErrConflict (serialization retries
	// exhausted) under 100-way single-tenant concurrency. It must now be zero.
	if f := failures.Load(); f != 0 {
		t.Fatalf("%d of %d single-tenant concurrent posts failed; expected zero now that same-tenant "+
			"posts serialize on the per-tenant in-process mutex", f, goroutines)
	}

	// Same core invariant as TestPostConcurrentStress: the ledger nets to zero.
	var total int64
	for _, id := range ids {
		bal, err := repo.Balance(ctx, tenant, id)
		if err != nil {
			t.Fatalf("balance %s: %v", id, err)
		}
		total += bal.Amount()
	}
	if total != 0 {
		t.Fatalf("ledger does not net to zero: sum of balances = %d", total)
	}

	// The chain itself must be genuinely valid, not merely error-free: every
	// row's stored hash must recompute from its own content and its
	// predecessor's hash, in order.
	result, err := auditSvc.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify audit chain: %v", err)
	}
	if !result.Valid {
		t.Fatalf("audit chain invalid: checked=%d first_break_id=%s", result.Checked, result.FirstBreakID)
	}
	if result.Checked != goroutines {
		t.Fatalf("audit chain checked %d rows, want %d (one per successful post)", result.Checked, goroutines)
	}
	t.Logf("posted %d transactions to a single tenant via %d fully concurrent goroutines; audit chain valid (checked=%d rows)",
		goroutines, goroutines, result.Checked)
}

// TestPostNoCrossTenantStarvation is the regression test for the second
// critical flaw in the DB-advisory-lock approach this rework replaced: a
// session advisory lock was taken on a connection checked out from the pool
// before the lock wait even began, and held for the whole call including
// every retry's backoff. A hot tenant's backlog of same-tenant posts could
// therefore check out and hold most or all of the pool's connections while
// merely waiting on the lock, starving every other tenant of a connection.
// That defeated the entire point of scoping the lock per tenant: different
// tenants are supposed to stay fully parallel.
//
// The fix (RunInTx holding a per-tenant in-process mutex, see
// internal/postgres/repository.go) never lets a waiter touch the pool: a
// goroutine blocked on another same-tenant call's mutex holds no database
// connection at all, and only acquires one once it is its turn to actually
// run an attempt. So a hot tenant's backlog, however long, never uses more
// than one connection at a time; the rest of a small pool stays free for
// other tenants throughout.
//
// This is checked by completion order, not wall-clock thresholds (which
// would be flaky across machines): a shared counter tracks how many of the
// hot tenant's posts have completed, and every "other" tenant's post records
// that counter's value the instant it completes. If other tenants were
// starved behind the hot backlog (the bug this replaces), that snapshot would
// be close to the full backlog size, since an other-tenant post could not get
// a connection until the backlog had largely drained. With the fix, other
// tenants should complete almost immediately, while the hot backlog's counter
// is still small.
func TestPostNoCrossTenantStarvation(t *testing.T) {
	const (
		// A deliberately scarce pool: small enough that the old bug (waiters
		// holding connections) would visibly starve other tenants, but with
		// at least one connection to spare for the fix to demonstrate leaves
		// free.
		starvationMaxConns = 2
		hotAccounts        = 6
		hotBacklog         = 80
		otherTenants       = 3
		postsPerOther      = 5
	)

	pool := newTestPoolWithMaxConns(t, starvationMaxConns)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	setupTenant := func(accounts int) (string, []string) {
		tenant := uuid.NewString()
		ids := make([]string, accounts)
		for i := range ids {
			a := &domain.Account{Name: "acct", Type: domain.Asset, Currency: "USD"}
			if err := repo.CreateAccount(ctx, tenant, a); err != nil {
				t.Fatalf("create account %d: %v", i, err)
			}
			ids[i] = a.ID
		}
		return tenant, ids
	}

	hotTenant, hotIDs := setupTenant(hotAccounts)
	otherTenantIDs := make([]string, otherTenants)
	otherAccountIDs := make([][]string, otherTenants)
	for i := range otherTenantIDs {
		otherTenantIDs[i], otherAccountIDs[i] = setupTenant(2)
	}

	post := func(tenant string, ids []string, seed int) error {
		rng := rand.New(rand.NewSource(int64(seed) + 1)) //nolint:gosec // test data, not crypto
		from := rng.Intn(len(ids))
		to := rng.Intn(len(ids) - 1)
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
		_, err := svc.Post(ctx, tenant, txn, nil)
		return err
	}

	var (
		wg          sync.WaitGroup
		hotFailures atomic.Int64
		hotDone     atomic.Int64
		othFailures atomic.Int64
		snapMu      sync.Mutex
		snapshots   = make([]int64, 0, otherTenants*postsPerOther)
	)

	// Launch the hot tenant's whole backlog at once. With the in-process
	// mutex these all serialize on Go: at most one is ever actually running
	// an attempt (and therefore holding a connection) at a time.
	for g := 0; g < hotBacklog; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			if err := post(hotTenant, hotIDs, seed); err != nil {
				hotFailures.Add(1)
				t.Errorf("hot tenant post failed: %v", err)
				return
			}
			hotDone.Add(1)
		}(g)
	}

	// A short head start so the hot backlog is genuinely queued up, and (if
	// the bug were present) would already be monopolizing the pool's
	// connections, before the other tenants start.
	time.Sleep(20 * time.Millisecond)

	for ti := 0; ti < otherTenants; ti++ {
		tenant, ids := otherTenantIDs[ti], otherAccountIDs[ti]
		for g := 0; g < postsPerOther; g++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				if err := post(tenant, ids, seed); err != nil {
					othFailures.Add(1)
					t.Errorf("other tenant post failed: %v", err)
					return
				}
				snap := hotDone.Load()
				snapMu.Lock()
				snapshots = append(snapshots, snap)
				snapMu.Unlock()
			}(ti*1000 + g)
		}
	}

	wg.Wait()

	if f := hotFailures.Load(); f != 0 {
		t.Fatalf("%d of %d hot-tenant posts failed", f, hotBacklog)
	}
	if f := othFailures.Load(); f != 0 {
		t.Fatalf("%d other-tenant posts failed", f)
	}
	if len(snapshots) != otherTenants*postsPerOther {
		t.Fatalf("got %d other-tenant completions, want %d", len(snapshots), otherTenants*postsPerOther)
	}

	// The property under test: other tenants complete while the hot backlog
	// is still mostly in flight, not after it has drained. A generous bound
	// (half the backlog) still cleanly separates "starved" from "not
	// starved": before this fix, a same-sized pool made every other-tenant
	// post wait for nearly the entire hot backlog to complete first, since
	// waiting hot goroutines had already checked out the pool's connections.
	var maxSnap int64
	for _, s := range snapshots {
		if s > maxSnap {
			maxSnap = s
		}
	}
	if limit := int64(hotBacklog) / 2; maxSnap >= limit {
		t.Fatalf("slowest other-tenant post did not complete until %d of %d hot-tenant posts had finished "+
			"(wanted under %d); other tenants appear to be starved behind the hot tenant's backlog",
			maxSnap, hotBacklog, limit)
	}
	t.Logf("hot tenant backlog=%d (pool MaxConns=%d); %d other-tenant posts across %d tenants all completed "+
		"while at most %d of the hot backlog had finished",
		hotBacklog, starvationMaxConns, otherTenants*postsPerOther, otherTenants, maxSnap)
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
