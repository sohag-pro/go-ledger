package audit_test

// Integration tests for the audit chainer (ADR-017), against a real
// testcontainers Postgres with the full goose migration set applied. One
// container is shared across this package's tests, started once in
// TestMain; tests skip cleanly (not fail) when no Docker daemon is reachable.
//
// None of these tests call t.Parallel(). Chainer.DrainOnce is documented as
// unsafe to call concurrently without Run's leader election coordinating,
// and several tests here deliberately exercise more than one Chainer against
// the shared pool; running them concurrently with each other would make that
// coordination itself racy to observe.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

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
	pool, err := postgres.NewPool(ctx, dsn, 25)
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

// newTestPool returns the shared pool, skipping the test when no container
// was available (for example no Docker), so the suite stays green without it.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return sharedPool
}

// newPool returns a fresh pool against the same shared container, with its
// own maxConns, closed on test cleanup. Used wherever a test needs a truly
// independent pool (standing in for a second app instance's own pool) rather
// than sharing the package-level one.
func newPool(t *testing.T, maxConns int32) *pgxpool.Pool {
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

// seedTenant creates a tenant with two USD accounts, returning the tenant id
// and both account ids (debit, credit).
func seedTenant(t *testing.T, repo *postgres.Repository) (tenant, debit, credit string) {
	t.Helper()
	ctx := context.Background()
	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "chainer test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	d := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	c := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, d); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, c); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	return tenant, d.ID, c.ID
}

// post posts one balanced transaction for tenant through the real
// TransactionService (so it writes an audit_outbox row, ADR-017, but no
// audit_log row), returning the transaction id.
func post(t *testing.T, svc *ledger.TransactionService, tenant, debit, credit string, amount int64) string {
	t.Helper()
	d, err := domain.NewMoney(amount, "USD")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	c, err := domain.NewMoney(-amount, "USD")
	if err != nil {
		t.Fatalf("new money: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit, Amount: d},
		{AccountID: credit, Amount: c},
	}}
	if _, err := svc.Post(context.Background(), tenant, txn, nil); err != nil {
		t.Fatalf("post transaction: %v", err)
	}
	return txn.ID
}

// drainUntilEmpty polls DrainOnce until tenant has no pending outbox rows, or
// fails the test after a generous timeout.
func drainUntilEmpty(t *testing.T, chainer *audit.Chainer, repo *postgres.Repository, tenant string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := chainer.DrainOnce(ctx); err != nil {
			t.Fatalf("drain: %v", err)
		}
		pending, err := repo.CountPendingOutbox(ctx, tenant)
		if err != nil {
			t.Fatalf("count pending outbox: %v", err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain timed out with %d rows still pending for tenant %s", pending, tenant)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestChainer_DrainsOutboxIntoValidChain proves the core mechanism end to
// end: posting writes an audit_outbox row, NOT an audit_log row (the chain
// does not exist yet); running the chainer drains it and produces a valid,
// verifiable chain.
func TestChainer_DrainsOutboxIntoValidChain(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	auditSvc := ledger.NewAuditService(repo)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo)
	const n = 5
	for i := 0; i < n; i++ {
		post(t, svc, tenant, debit, credit, int64(100+i))
	}

	// Before the chainer runs: nothing in audit_log yet, everything pending.
	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("audit_log rows before draining = %d, want 0 (post only writes the outbox, ADR-017)", len(rows))
	}
	pending, err := repo.CountPendingOutbox(ctx, tenant)
	if err != nil {
		t.Fatalf("count pending outbox: %v", err)
	}
	if pending != n {
		t.Fatalf("pending outbox rows = %d, want %d", pending, n)
	}

	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repo, tenant)

	result, err := auditSvc.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Fatalf("chain invalid after drain: checked=%d first_break=%s", result.Checked, result.FirstBreakID)
	}
	if result.Checked != n {
		t.Fatalf("checked = %d, want %d", result.Checked, n)
	}
	if result.Pending != 0 {
		t.Fatalf("pending after drain = %d, want 0", result.Pending)
	}
}

// TestChainer_PreservesOccurredAt proves the chainer reproduces today's
// row_hash bit for bit: the chained row's CreatedAt must be exactly the
// outbox row's occurred_at (read back from the database, not recomputed),
// and recomputing domain.ComputeAuditRowHash from the chained row's own
// stored content must match its stored RowHash.
func TestChainer_PreservesOccurredAt(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo)
	txnID := post(t, svc, tenant, debit, credit, 250)

	var wantOccurredAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT occurred_at FROM audit_outbox WHERE transaction_id = $1`, uuid.MustParse(txnID),
	).Scan(&wantOccurredAt); err != nil {
		t.Fatalf("read outbox occurred_at: %v", err)
	}

	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repo, tenant)

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if !row.CreatedAt.Equal(wantOccurredAt) {
		t.Errorf("chained row CreatedAt = %v, want the outbox row's occurred_at %v", row.CreatedAt, wantOccurredAt)
	}
	if recomputed := domain.ComputeAuditRowHash(tenant, row, domain.AuditGenesisHash); recomputed != row.RowHash {
		t.Errorf("recomputed row_hash %q != stored %q: occurred_at was not preserved bit-for-bit into the hash", recomputed, row.RowHash)
	}
}

// TestChainer_IdempotentOnRerun proves running the chainer again after a
// full drain does nothing: no new audit_log rows, the existing ones
// untouched, pending still zero.
func TestChainer_IdempotentOnRerun(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo)
	const n = 4
	for i := 0; i < n; i++ {
		post(t, svc, tenant, debit, credit, int64(50+i))
	}

	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repo, tenant)

	before, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify (before rerun): %v", err)
	}
	if len(before) != n {
		t.Fatalf("audit rows before rerun = %d, want %d", len(before), n)
	}

	// Run DrainOnce several more times: nothing left to do, nothing changes.
	for i := 0; i < 3; i++ {
		processed, err := chainer.DrainOnce(ctx)
		if err != nil {
			t.Fatalf("drain rerun %d: %v", i, err)
		}
		if processed != 0 {
			t.Fatalf("drain rerun %d processed %d rows, want 0 (already fully drained)", i, processed)
		}
	}

	after, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify (after rerun): %v", err)
	}
	if len(after) != n {
		t.Fatalf("audit rows after rerun = %d, want %d (no double-chaining)", len(after), n)
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].RowHash != after[i].RowHash || before[i].PrevHash != after[i].PrevHash {
			t.Fatalf("row %d changed across reruns: before=%+v after=%+v", i, before[i], after[i])
		}
	}
	pending, err := repo.CountPendingOutbox(ctx, tenant)
	if err != nil {
		t.Fatalf("count pending outbox: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending after rerun = %d, want 0", pending)
	}
}

// TestChainer_XminOrderingHoldsBackInFlightTransaction proves the core
// ordering guarantee (ADR-017 section 2): a row inserted by a still-open
// transaction is NOT processed while that transaction remains in flight,
// even though a LATER-committing row exists; once the held transaction
// commits, the watermark advances and the row is chained in the correct
// (txid, id) position.
//
// Mechanics: a holder transaction begins on its own connection and is kept
// open (never committed) for most of the test. A second, ordinary post
// commits normally after the holder began, so its outbox row's txid is
// greater than the holder's, and the holder being still in flight pins
// pg_snapshot_xmin at or below the holder's own txid: the second row's txid
// is not below that watermark, so a drain must skip it. Only after the
// holder itself commits (or rolls back) does the watermark advance past it,
// making the second row eligible.
func TestChainer_XminOrderingHoldsBackInFlightTransaction(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	auditSvc := ledger.NewAuditService(repo)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repo)

	// Seed one row before the holder even begins, so there is a genesis link
	// to chain off of once the second row becomes eligible.
	post(t, svc, tenant, debit, credit, 111)

	// The holder: begins a real transaction and keeps it open. It does not
	// need to touch audit_outbox itself; merely being open is what pins the
	// xmin watermark.
	holderTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	// A trivial statement to make sure the transaction has actually been
	// assigned a real transaction id server-side (a transaction with no
	// writes at all can, in principle, stay purely read-only and never get
	// a txid; forcing a write guarantees pg_current_xact_id() has fired).
	if _, err := holderTx.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatalf("assign holder txid: %v", err)
	}
	holderReleased := false
	release := func() {
		if holderReleased {
			return
		}
		holderReleased = true
		_ = holderTx.Rollback(context.Background())
	}
	defer release()

	// Now post the second transaction, on the repo's own pool (a different
	// connection than the holder's): it commits normally and gets a txid
	// greater than the holder's, since the holder started first.
	post(t, svc, tenant, debit, credit, 222)

	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)

	// Drain while the holder is still open: only the first row (seeded
	// before the holder began) is guaranteed committed and below the
	// watermark. The second row's txid is not below xmin (the holder is
	// still the oldest in-flight transaction), so it must NOT be processed.
	if _, err := chainer.DrainOnce(ctx); err != nil {
		t.Fatalf("drain while holder open: %v", err)
	}
	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify (holder open): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows while holder open = %d, want 1 (the second row must be held back)", len(rows))
	}
	pending, err := repo.CountPendingOutbox(ctx, tenant)
	if err != nil {
		t.Fatalf("count pending outbox (holder open): %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending while holder open = %d, want 1 (the second row, held back by xmin)", pending)
	}

	// Release the holder: the watermark can now advance past it.
	release()

	// Drain again: the second row is now eligible and must chain correctly
	// off the first row.
	drainUntilEmpty(t, chainer, repo, tenant)

	result, err := auditSvc.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Fatalf("chain invalid after holder released: checked=%d first_break=%s", result.Checked, result.FirstBreakID)
	}
	if result.Checked != 2 {
		t.Fatalf("checked = %d, want 2", result.Checked)
	}
}

// TestChainer_LeaderElection_OnlyOneDrainsAtATime proves exactly one of two
// Chainer instances, run concurrently against the same database via Run
// (not DrainOnce), ever drains at a time: two independent pools (standing in
// for two app instances) both run a chainer, one posts a real burst of
// events for one tenant, and the resulting chain, once fully drained, must
// be a single unbroken sequence with no fork. A fork here could only happen
// if both chainers were inserting into audit_log at once, which leader
// election (the session advisory lock) is supposed to prevent.
//
// It also proves failover: after the first chainer's context is cancelled
// (and its Run goroutine has fully returned, releasing its advisory lock),
// a further burst of events is posted and must still drain, necessarily by
// the second chainer, since the first is no longer running at all.
func TestChainer_LeaderElection_OnlyOneDrainsAtATime(t *testing.T) {
	poolA := newPool(t, 10)
	poolB := newPool(t, 10)
	repo := postgres.NewRepository(poolA)
	svc := ledger.NewTransactionService(repo, discardLogger(), nil)
	auditSvc := ledger.NewAuditService(repo)

	tenant, debit, credit := seedTenant(t, repo)

	chainerA := audit.NewChainer(poolA, discardLogger(), 20*time.Millisecond, 50)
	chainerB := audit.NewChainer(poolB, discardLogger(), 20*time.Millisecond, 50)

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() { defer close(doneA); chainerA.Run(ctxA) }()

	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan struct{})
	go func() { defer close(doneB); chainerB.Run(ctxB) }()
	defer func() { cancelB(); <-doneB }()

	const batch1 = 25
	for i := 0; i < batch1; i++ {
		post(t, svc, tenant, debit, credit, int64(300+i))
	}
	waitForPendingZero(t, repo, tenant)

	// Stop chainer A and wait for its goroutine to fully return: only then
	// is it guaranteed to have released its advisory lock (or never held
	// it), so any further work can only be drained by chainer B.
	cancelA()
	<-doneA

	const batch2 = 25
	for i := 0; i < batch2; i++ {
		post(t, svc, tenant, debit, credit, int64(400+i))
	}
	waitForPendingZero(t, repo, tenant)

	result, err := auditSvc.Verify(context.Background(), tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Fatalf("chain invalid: checked=%d first_break=%s", result.Checked, result.FirstBreakID)
	}
	if result.Checked != batch1+batch2 {
		t.Fatalf("checked = %d, want %d", result.Checked, batch1+batch2)
	}
	assertNoFork(t, repo, tenant, batch1+batch2)
}

// waitForPendingZero polls CountPendingOutbox until it reaches zero (the
// chainer(s) running in the background are expected to drain it), or fails
// the test after a generous timeout.
func waitForPendingZero(t *testing.T, repo *postgres.Repository, tenant string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for {
		pending, err := repo.CountPendingOutbox(ctx, tenant)
		if err != nil {
			t.Fatalf("count pending outbox: %v", err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for pending outbox to drain: %d rows still pending", pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoFork proves the tenant's audit_log rows form a single linear
// chain: every prev_hash value, including genesis, is claimed by exactly
// one row. A fork would show up as some prev_hash value shared by two or
// more rows (two different "next" links both claiming the same
// predecessor). It also checks the row count and that no outbox row is left
// unprocessed.
func assertNoFork(t *testing.T, repo *postgres.Repository, tenant string, wantRows int) {
	t.Helper()
	ctx := context.Background()
	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != wantRows {
		t.Fatalf("audit_log rows = %d, want %d", len(rows), wantRows)
	}
	prevCounts := make(map[string]int, len(rows))
	for _, r := range rows {
		prevCounts[r.PrevHash]++
	}
	for prev, count := range prevCounts {
		if count > 1 {
			label := prev
			if label == domain.AuditGenesisHash {
				label = "<genesis>"
			}
			t.Errorf("prev_hash %q is claimed by %d rows, want at most 1 (a fork)", label, count)
		}
	}
	pending, err := repo.CountPendingOutbox(ctx, tenant)
	if err != nil {
		t.Fatalf("count pending outbox: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending outbox rows = %d, want 0 (every posted event must be processed)", pending)
	}
}

// TestChainer_TwoInstanceNoFork is the acceptance gate for this ADR (A8.2):
// two independent Repository instances, each with its own pool (exactly as
// two separate app instances would each have their own pool), post K
// concurrent transactions each (2K total) for the SAME tenant. Neither
// instance's RunInTx holds any per-tenant mutex anymore (ADR-017 removes
// it), and neither ever reads or extends the audit chain: both merely write
// outbox rows. After every post has committed, a single chainer drains the
// outbox. The resulting chain must have exactly 2K rows, verify valid, and
// show no fork: this is the concrete proof that the single-instance
// correctness cliff (audit A3.6, the Blocker this ADR closes) no longer
// applies. See the Task 3.2 report for the documented BASELINE run against
// the pre-ADR-017 design (this same scenario, run against the code before
// this change), which reproduces a serialization-storm failure under
// two-instance same-tenant contention that this design does not.
func TestChainer_TwoInstanceNoFork(t *testing.T) {
	poolA := newPool(t, 20)
	poolB := newPool(t, 20)
	repoA := postgres.NewRepository(poolA)
	repoB := postgres.NewRepository(poolB)
	svcA := ledger.NewTransactionService(repoA, discardLogger(), nil)
	svcB := ledger.NewTransactionService(repoB, discardLogger(), nil)
	auditSvc := ledger.NewAuditService(repoA)
	ctx := context.Background()

	tenant, debit, credit := seedTenant(t, repoA)

	const perInstance = 150
	var wg sync.WaitGroup
	var failMu sync.Mutex
	var failures []error
	runBatch := func(svc *ledger.TransactionService, base int) {
		defer wg.Done()
		for i := 0; i < perInstance; i++ {
			d, _ := domain.NewMoney(int64(base+i+1), "USD")
			c, _ := domain.NewMoney(-int64(base+i+1), "USD")
			txn := &domain.Transaction{Postings: []domain.Posting{
				{AccountID: debit, Amount: d},
				{AccountID: credit, Amount: c},
			}}
			if _, err := svc.Post(ctx, tenant, txn, nil); err != nil {
				failMu.Lock()
				failures = append(failures, err)
				failMu.Unlock()
			}
		}
	}
	wg.Add(2)
	go runBatch(svcA, 0)
	go runBatch(svcB, 1_000_000)
	wg.Wait()

	if len(failures) != 0 {
		t.Fatalf("%d of %d two-instance concurrent posts failed (want zero): first error: %v",
			len(failures), 2*perInstance, failures[0])
	}

	// A single chainer drains everything both instances wrote.
	chainer := audit.NewChainer(poolA, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repoA, tenant)

	result, err := auditSvc.Verify(ctx, tenant)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !result.Valid {
		t.Fatalf("chain invalid: checked=%d first_break=%s (a fork would surface here)", result.Checked, result.FirstBreakID)
	}
	if result.Checked != 2*perInstance {
		t.Fatalf("checked = %d, want %d", result.Checked, 2*perInstance)
	}
	assertNoFork(t, repoA, tenant, 2*perInstance)

	t.Logf("two-instance no-fork acceptance: %d posts (%d per instance) across 2 independent pools, "+
		"chain valid and unforked after a single chainer drain", 2*perInstance, perInstance)
}

// TestChainer_ReuseAuditEntryJSONRoundTrip is a small sanity check that
// Before/After survive the outbox -> audit_log round trip unchanged (the
// json/jsonb distinction migration 0009 cared about for audit_log applies
// equally to values that pass through audit_outbox first).
func TestChainer_ReuseAuditEntryJSONRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant, _, _ := seedTenant(t, repo)

	txID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txID, tenant); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	after, err := json.Marshal(map[string]any{"id": txID, "note": "round trip"})
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	err = repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAuditOutbox(ctx, tenant, domain.AuditEvent{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txID,
			Actor:         tenant,
			After:         after,
		})
	})
	if err != nil {
		t.Fatalf("append audit outbox: %v", err)
	}

	chainer := audit.NewChainer(pool, discardLogger(), time.Millisecond, 500)
	drainUntilEmpty(t, chainer, repo, tenant)

	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if string(rows[0].After) != string(after) {
		t.Errorf("after = %q, want %q (byte-exact round trip)", rows[0].After, after)
	}
}
