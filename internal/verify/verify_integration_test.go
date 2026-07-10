package verify_test

// Integration tests for internal/verify, against a real testcontainers
// Postgres with the full goose migration set applied. This follows the same
// container/migration/skip pattern as internal/seed's and internal/ledger's
// integration suites (see internal/seed/seed_test.go, internal/ledger/stress_test.go):
// one shared container started in TestMain, tests skip cleanly (not fail)
// when no Docker daemon is reachable (for example TESTCONTAINERS or
// DOCKER_HOST unset, or colima not running).
//
// Each test posts data through the real services (ledger.AccountService,
// ledger.TransactionService), exactly as production would, and only then
// corrupts it with a direct, privileged write that bypasses the application:
// this is what proves the corruption is real and that verify.Run actually
// discriminates a healthy ledger from a broken one, rather than always
// passing or always failing.
//
// None of these tests call t.Parallel(): verify.Run scans the whole database
// (every transaction's postings, every tenant's audit chain), by design,
// since the real tool runs once against a whole restored database, not
// scoped to one tenant. Running these tests concurrently against the shared
// pool would let one test's corruption or cleanup race another test's global
// scan. Each test restores whatever it corrupts before returning, so the
// suite still leaves a clean shared pool for any test added later.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	"github.com/sohag-pro/go-ledger/internal/verify"
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
		// Wait on the readiness log, not the open port: Postgres opens 5432
		// during initdb then restarts it, so a port-only wait races real
		// readiness. The log appears twice, hence WithOccurrence(2).
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
	pool, err := postgres.NewPool(ctx, dsn, 10)
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

// newTestPool returns the shared pool, skipping the test when no container
// was available, so the suite stays green on a machine without Docker.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return sharedPool
}

// postTxn posts one balanced two-leg transaction for tenant through the real
// account and transaction services, so it creates both a well-formed posting
// pair and a real, hash-chained audit row, exactly as production would.
func postTxn(t *testing.T, pool *pgxpool.Pool, tenant string) string {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	txns := ledger.NewTransactionService(repo, nil, nil)

	if err := repo.CreateTenant(ctx, tenant, "verify test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, cash); err != nil {
		t.Fatalf("create cash account: %v", err)
	}
	if err := accounts.Create(ctx, tenant, revenue); err != nil {
		t.Fatalf("create revenue account: %v", err)
	}

	debit, err := domain.NewMoney(500, "USD")
	if err != nil {
		t.Fatalf("new debit money: %v", err)
	}
	credit, err := domain.NewMoney(-500, "USD")
	if err != nil {
		t.Fatalf("new credit money: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: cash.ID, Amount: debit},
		{AccountID: revenue.ID, Amount: credit},
	}}
	if _, err := txns.Post(ctx, tenant, txn, nil); err != nil {
		t.Fatalf("post transaction: %v", err)
	}
	return txn.ID
}

// TestRun_HealthyLedgerPasses proves a healthy, real ledger (accounts, a
// balanced transaction, and its accompanying audit row, all written through
// the real services) verifies clean: no balance violations, no chain breaks,
// and at least one tenant actually checked.
func TestRun_HealthyLedgerPasses(t *testing.T) {
	pool := newTestPool(t)
	tenant := uuid.NewString()
	postTxn(t, pool, tenant)

	report, err := verify.Run(context.Background(), pool)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Fatalf("report = %+v, want OK() == true", report)
	}
	if report.TenantsChecked < 1 {
		t.Errorf("tenants checked = %d, want >= 1", report.TenantsChecked)
	}
	if len(report.BalanceViolations) != 0 {
		t.Errorf("balance violations = %v, want none", report.BalanceViolations)
	}
	if len(report.ChainBreaks) != 0 {
		t.Errorf("chain breaks = %v, want none", report.ChainBreaks)
	}
}

// TestRun_BrokenBalanceFails posts a real, balanced transaction and then
// directly corrupts one of its postings with `UPDATE postings SET amount =
// amount + 1`. The balance trigger (assert_txn_balanced, see
// internal/postgres/migrations/0002_balance_trigger.sql) only fires AFTER
// INSERT, never AFTER UPDATE, so this direct write genuinely leaves the row
// unbalanced in the database rather than being caught and rolled back. Run
// must catch it: exactly one BalanceViolation for that transaction and
// currency, and a non-OK report.
func TestRun_BrokenBalanceFails(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID := postTxn(t, pool, tenant)

	// Sanity: the ledger verifies clean before any corruption.
	before, err := verify.Run(ctx, pool)
	if err != nil {
		t.Fatalf("Run before corruption: %v", err)
	}
	if !before.OK() {
		t.Fatalf("report before corruption = %+v, want OK() == true", before)
	}

	var postingID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM postings WHERE transaction_id = $1 ORDER BY id LIMIT 1`,
		uuid.MustParse(txnID),
	).Scan(&postingID); err != nil {
		t.Fatalf("find a posting to corrupt: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE postings SET amount = amount + 1 WHERE id = $1`, postingID,
	); err != nil {
		t.Fatalf("corrupt posting amount: %v", err)
	}
	// Restore afterward so the shared pool's state does not leak into other
	// parallel tests reading across the whole postings table.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE postings SET amount = amount - 1 WHERE id = $1`, postingID,
		); err != nil {
			t.Errorf("restore corrupted posting: %v", err)
		}
	})

	report, err := verify.Run(ctx, pool)
	if err != nil {
		t.Fatalf("Run after corruption: %v", err)
	}
	if report.OK() {
		t.Fatal("report.OK() = true after corrupting a posting, want false")
	}
	if len(report.BalanceViolations) != 1 {
		t.Fatalf("balance violations = %d, want exactly 1: %+v", len(report.BalanceViolations), report.BalanceViolations)
	}
	v := report.BalanceViolations[0]
	if v.TransactionID != txnID {
		t.Errorf("violation transaction id = %q, want %q", v.TransactionID, txnID)
	}
	if v.Currency != "USD" {
		t.Errorf("violation currency = %q, want USD", v.Currency)
	}
	if v.Sum != 1 {
		t.Errorf("violation sum = %d, want 1 (one leg pushed +1 off balance)", v.Sum)
	}
}

// TestRun_TamperedAuditChainFails posts a real transaction (which writes one
// real, hash-chained audit row) and then tampers with that row's row_hash
// directly, bypassing the audit_log immutability trigger the same way the
// existing postgres and ledger hash-chain tests do (SET LOCAL
// audit.allow_purge = 'on', the seeder's own escape hatch; the application
// path never sets this). Run must report a ChainBreak for the tenant with the
// tampered row as FirstBreakID, and a non-OK report.
func TestRun_TamperedAuditChainFails(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	postTxn(t, pool, tenant)

	repo := postgres.NewRepository(pool)
	rows, err := repo.ListAuditForVerify(ctx, tenant)
	if err != nil {
		t.Fatalf("list audit for verify: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	row := rows[0]

	tamperTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tamper tx: %v", err)
	}
	defer tamperTx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if _, err := tamperTx.Exec(ctx, `SET LOCAL audit.allow_purge = 'on'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := tamperTx.Exec(ctx,
		`UPDATE audit_log SET row_hash = 'tampered' WHERE id = $1`, uuid.MustParse(row.ID),
	); err != nil {
		t.Fatalf("tamper row_hash: %v", err)
	}
	if err := tamperTx.Commit(ctx); err != nil {
		t.Fatalf("commit tamper: %v", err)
	}
	// Restore the original row_hash afterward so this test's corruption does
	// not leak into other tests reading across the whole audit_log table
	// (verify.Run's chain check is global, not scoped to this test's tenant).
	t.Cleanup(func() {
		restoreTx, err := pool.Begin(context.Background())
		if err != nil {
			t.Errorf("begin restore tx: %v", err)
			return
		}
		defer restoreTx.Rollback(context.Background()) //nolint:errcheck // no-op after commit
		if _, err := restoreTx.Exec(context.Background(), `SET LOCAL audit.allow_purge = 'on'`); err != nil {
			t.Errorf("set local for restore: %v", err)
			return
		}
		if _, err := restoreTx.Exec(context.Background(),
			`UPDATE audit_log SET row_hash = $1 WHERE id = $2`, row.RowHash, uuid.MustParse(row.ID),
		); err != nil {
			t.Errorf("restore tampered row_hash: %v", err)
			return
		}
		if err := restoreTx.Commit(context.Background()); err != nil {
			t.Errorf("commit restore: %v", err)
		}
	})

	report, err := verify.Run(ctx, pool)
	if err != nil {
		t.Fatalf("Run after tampering: %v", err)
	}
	if report.OK() {
		t.Fatal("report.OK() = true after tampering with row_hash, want false")
	}
	if len(report.ChainBreaks) != 1 {
		t.Fatalf("chain breaks = %d, want exactly 1: %+v", len(report.ChainBreaks), report.ChainBreaks)
	}
	b := report.ChainBreaks[0]
	if b.TenantID != tenant {
		t.Errorf("chain break tenant = %q, want %q", b.TenantID, tenant)
	}
	if b.FirstBreakID != row.ID {
		t.Errorf("chain break first break id = %q, want the tampered row %q", b.FirstBreakID, row.ID)
	}
	if b.Checked != 1 {
		t.Errorf("chain break checked = %d, want 1", b.Checked)
	}
}
