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
// TestPostConcurrentStressSingleTenant for that one, which since ADR-017
// removed RunInTx's per-tenant mutex, and covers the property this test
// does not: many fully concurrent posts to the SAME tenant).
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
		if err := repo.CreateTenant(ctx, tenants[tn], "stress test tenant"); err != nil {
			t.Fatalf("create tenant %d: %v", tn, err)
		}
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
// TestPostConcurrentStress. Before ADR-017, every post read the tenant's
// latest audit row_hash and inserted the next row inside the same
// transaction (ADR-012's hash chain), so concurrent same-tenant posts raced
// on that read; at 100-way concurrency PostgreSQL's SERIALIZABLE aborts
// could exhaust the 25-attempt retry budget, and RunInTx held a per-tenant
// in-process mutex to serialize same-tenant posts and stop that. ADR-017
// removes the chain read from the posting transaction entirely (a post now
// writes an audit_outbox row; a single background chainer builds the chain
// asynchronously), which removed the reason for the mutex, so these 100
// posts now run fully concurrently instead of serializing, and this test is
// the regression check that removing the mutex did not reintroduce the
// retry-exhaustion failure it used to fix: every one of them must still
// succeed. After draining the chainer, it also walks the tenant's full audit
// chain (AuditService.Verify) to prove the chainer, run after the fact,
// still produces a genuinely unbroken, correctly ordered hash chain covering
// every post, not just "no post errors returned."
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
	if err := repo.CreateTenant(ctx, tenant, "stress test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
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

	// The crux of this test: before ADR-017 removed the audit chain read from
	// the posting transaction, 100-way single-tenant concurrency reliably
	// failed with domain.ErrConflict (serialization retries exhausted)
	// unless a per-tenant mutex serialized these posts. It must still be
	// zero now that the mutex is gone, since removing the chain read (not
	// the mutex) is what actually removed the conflict source.
	if f := failures.Load(); f != 0 {
		t.Fatalf("%d of %d single-tenant concurrent posts failed; expected zero since the audit chain read "+
			"(the old conflict source) is no longer part of the posting transaction", f, goroutines)
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

	// Post only writes an audit_outbox row (ADR-017); drain the chainer so
	// there is a chain to verify at all.
	drainChainer(t, pool, tenant)

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

// TestPostConcurrentContentionDoesNotCorruptLedger replaces what used to be
// TestPostNoCrossTenantStarvation. That test asserted a fairness property a
// per-tenant in-process mutex used to provide: a hot tenant's backlog of
// same-tenant posts, however long, would never hold more than one pool
// connection at a time, so a small pool's remaining connections stayed free
// for other tenants. ADR-017 removes that mutex (it existed to protect the
// now-removed synchronous audit chain read, and an in-process mutex could
// never coordinate fairness across more than one app instance regardless,
// which the mutex predated needing to do). Its removal is a genuine,
// verified trade-off, not a fixed bug: with the mutex gone, this same
// scenario (a small pool, one tenant bursting far more concurrent posts than
// there are connections) now lets that tenant's backlog queue up for
// connections ahead of other tenants, and this repo's test suite confirmed
// exactly that (a run of the old test against this change reliably fails
// its fairness bound: the slowest other-tenant post did not complete until
// the ENTIRE 80-post hot backlog had drained, not merely half of it).
//
// This is judged an acceptable trade-off for now (see the audit
// remediation's Task 3.2 report for the fuller discussion): production
// pools are sized far larger than this deliberately tiny 2-connection
// scenario, and fair queueing across tenants under connection-pool pressure
// is a separate, unaddressed concern from the audit chain's multi-instance
// correctness, not something ADR-017 set out to fix or preserve. It is
// flagged here for whoever revisits connection-pool sizing or fairness
// later, rather than silently dropped.
//
// What this test keeps checking is the property that still matters: even
// under exactly the contention that now produces unfair queueing, no post
// is lost or corrupted. Every post (hot tenant and other tenants alike)
// still succeeds, and the ledger still nets to zero.
func TestPostConcurrentContentionDoesNotCorruptLedger(t *testing.T) {
	const (
		// The same deliberately scarce pool the old fairness test used: small
		// enough that connection-pool contention is guaranteed, which is
		// exactly the condition this test now wants (see the doc comment
		// above): it is not asserting fairness anymore, only that contention
		// this severe still cannot corrupt the ledger.
		contentionMaxConns = 2
		hotAccounts        = 6
		hotBacklog         = 80
		otherTenants       = 3
		postsPerOther      = 5
	)

	pool := newTestPoolWithMaxConns(t, contentionMaxConns)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	setupTenant := func(accounts int) (string, []string) {
		tenant := uuid.NewString()
		if err := repo.CreateTenant(ctx, tenant, "stress test tenant"); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
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
		othFailures atomic.Int64
	)

	// Launch the hot tenant's whole backlog at once: with no per-tenant
	// mutex, all 80 immediately contend for the pool's 2 connections.
	for g := 0; g < hotBacklog; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			if err := post(hotTenant, hotIDs, seed); err != nil {
				hotFailures.Add(1)
				t.Errorf("hot tenant post failed: %v", err)
			}
		}(g)
	}

	// A short head start so the hot backlog is genuinely queued up ahead of
	// the other tenants, deliberately the worst case for the other tenants'
	// queueing position (see the doc comment above: this is what now
	// produces the accepted-tradeoff unfairness, not a bug in itself).
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
				}
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

	// The property that still matters: no post was lost or corrupted by the
	// contention, hot tenant or otherwise. Each tenant's own accounts net to
	// zero independently (no cross-tenant posting is possible).
	for _, tenant := range append([]string{hotTenant}, otherTenantIDs...) {
		ids := hotIDs
		if tenant != hotTenant {
			ids = otherAccountIDs[indexOf(otherTenantIDs, tenant)]
		}
		var total int64
		for _, id := range ids {
			bal, err := repo.Balance(ctx, tenant, id)
			if err != nil {
				t.Fatalf("balance %s (tenant %s): %v", id, tenant, err)
			}
			total += bal.Amount()
		}
		if total != 0 {
			t.Fatalf("tenant %s does not net to zero under contention: sum of balances = %d", tenant, total)
		}
	}
	t.Logf("hot tenant backlog=%d (pool MaxConns=%d) plus %d other-tenant posts across %d tenants all "+
		"completed correctly under heavy connection-pool contention", hotBacklog, contentionMaxConns,
		otherTenants*postsPerOther, otherTenants)
}

// indexOf returns the index of needle in haystack, or -1 if absent.
func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
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

	// tenants first: accounts_tenant_fk (migration 0011) requires the tenant
	// row to exist before an account can reference it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, 'test tenant')`, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
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
		`INSERT INTO transactions (id, tenant_id) VALUES ($1,$2)`,
		txn, tenant); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	// A single non-zero posting: the transaction cannot possibly balance.
	if _, err := tx.Exec(ctx,
		`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, currency) VALUES ($1,$2,$3,$4,$5,'USD')`,
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
