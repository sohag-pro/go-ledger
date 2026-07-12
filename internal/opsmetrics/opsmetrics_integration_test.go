package opsmetrics_test

// Integration tests for internal/opsmetrics, against a real testcontainers
// Postgres with the full goose migration set applied. This follows the same
// container/migration/skip pattern as internal/verify's own integration
// suite (see verify_integration_test.go): one shared container started in
// TestMain, tests skip cleanly (not fail) when no Docker daemon is reachable
// (colima not running, DOCKER_HOST unset, etc).
//
// internal/opsmetrics.Collector reads cross-tenant aggregates, exactly like
// internal/verify's restore-verifier, so these tests follow verify's own
// discipline: no t.Parallel() (every test reads the whole database's
// audit_outbox/webhook_deliveries/postings state, not a single tenant's),
// and every test restores or deletes whatever it inserted or corrupted via
// t.Cleanup, so the shared pool is clean again for the next test.
//
// The balance-invariant canary test is the important one: it proves
// ledger_balance_invariant_violations is 0 against a real, healthy ledger,
// then strictly positive the moment a posting is corrupted the same way
// internal/verify's own TestRun_BrokenBalanceFails proves it (a direct
// UPDATE, which the assert_txn_balanced trigger cannot catch because it only
// fires AFTER INSERT, never AFTER UPDATE).

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/audit"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/metrics"
	"github.com/sohag-pro/go-ledger/internal/opsmetrics"
	"github.com/sohag-pro/go-ledger/internal/postgres"
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// postTxn posts one balanced two-leg transaction for a fresh tenant through
// the real account and transaction services, exactly as production would,
// leaving one audit_outbox row UNDRAINED (ADR-017: Post only writes the
// outbox row; a separate chainer builds audit_log). Callers that want a
// drained chain call drainChainer themselves afterward.
func postTxn(t *testing.T, pool *pgxpool.Pool) (tenant, txnID string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRepository(pool)
	accounts := ledger.NewAccountService(repo)
	txns := ledger.NewTransactionService(repo, nil, nil)

	tenant = uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "opsmetrics test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := accounts.Create(ctx, tenant, cash, nil); err != nil {
		t.Fatalf("create cash account: %v", err)
	}
	if err := accounts.Create(ctx, tenant, revenue, nil); err != nil {
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
	return tenant, txn.ID
}

// drainChainer runs a real Chainer against pool until tenant has no pending
// outbox rows left, or fails the test after a generous timeout. Mirrors
// internal/verify's own drainChainer helper (chainer_helper_test.go).
func drainChainer(t *testing.T, pool *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewRepository(pool)
	chainer := audit.NewChainer(pool, testLogger(), time.Millisecond, 500)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := chainer.DrainOnce(ctx); err != nil {
			t.Fatalf("drain audit outbox: %v", err)
		}
		pending, err := repo.CountPendingOutbox(ctx, tenant)
		if err != nil {
			t.Fatalf("count pending outbox: %v", err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("draining audit outbox for tenant %s timed out with %d rows still pending", tenant, pending)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCollector_Integration_AuditOutboxBacklog proves
// ledger_audit_outbox_pending and ledger_audit_outbox_lag_seconds reflect a
// real, undrained audit_outbox row: posting a transaction leaves exactly one
// unprocessed row (ADR-017), backdating it makes the lag observable
// deterministically rather than racing the clock, and draining the chainer
// affterward returns both gauges to a value consistent with no pending work
// for that row.
func TestCollector_Integration_AuditOutboxBacklog(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tenant, txnID := postTxn(t, pool)
	t.Cleanup(func() { drainChainer(t, pool, tenant) })

	// Backdate the outbox row's created_at so the lag this produces is large
	// and unambiguous, rather than however many milliseconds this test
	// happened to take.
	tag, err := pool.Exec(ctx,
		`UPDATE audit_outbox SET created_at = now() - interval '10 minutes' WHERE transaction_id = $1`,
		uuid.MustParse(txnID),
	)
	if err != nil {
		t.Fatalf("backdate outbox row: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdate outbox row affected %d rows, want 1", tag.RowsAffected())
	}

	c := opsmetrics.NewCollector(pool, testLogger())
	c.Collect(ctx)

	if got := testutil.ToFloat64(metrics.AuditOutboxPending); got < 1 {
		t.Errorf("AuditOutboxPending = %v, want >= 1 with an undrained outbox row", got)
	}
	if got := testutil.ToFloat64(metrics.AuditOutboxLagSeconds); got < 590 {
		t.Errorf("AuditOutboxLagSeconds = %v, want >= 590 (row backdated ~10m)", got)
	}
}

// TestCollector_Integration_WebhookBacklog proves
// ledger_webhook_deliveries_dead and ledger_webhook_deliveries_pending
// reflect real webhook_deliveries rows in those states.
func TestCollector_Integration_WebhookBacklog(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := postgres.NewRepository(pool)

	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "opsmetrics webhook test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	subID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_subscriptions (id, tenant_id, url, secret, event_types, active)
		VALUES ($1, $2, 'https://example.invalid/hook', 'test-secret', '{}', true)
	`, subID, uuid.MustParse(tenant)); err != nil {
		t.Fatalf("insert webhook subscription: %v", err)
	}

	deadID := uuid.New()
	pendingID := uuid.New()
	insertDelivery := func(id uuid.UUID, chainSeq int64, status string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO webhook_deliveries
				(id, tenant_id, subscription_id, audit_chain_seq, event_type, payload, status)
			VALUES ($1, $2, $3, $4, 'transaction.posted', '{}'::jsonb, $5)
		`, id, uuid.MustParse(tenant), subID, chainSeq, status); err != nil {
			t.Fatalf("insert webhook delivery (status=%s): %v", status, err)
		}
	}
	insertDelivery(deadID, 1, "dead")
	insertDelivery(pendingID, 2, "pending")
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM webhook_deliveries WHERE id IN ($1, $2)`, deadID, pendingID,
		); err != nil {
			t.Errorf("cleanup webhook deliveries: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM webhook_subscriptions WHERE id = $1`, subID,
		); err != nil {
			t.Errorf("cleanup webhook subscription: %v", err)
		}
	})

	c := opsmetrics.NewCollector(pool, testLogger())
	c.Collect(ctx)

	if got := testutil.ToFloat64(metrics.WebhookDeliveriesDead); got < 1 {
		t.Errorf("WebhookDeliveriesDead = %v, want >= 1", got)
	}
	if got := testutil.ToFloat64(metrics.WebhookDeliveriesPending); got < 1 {
		t.Errorf("WebhookDeliveriesPending = %v, want >= 1", got)
	}
}

// TestCollector_Integration_BalanceInvariantCanary is the important test:
// it proves ledger_balance_invariant_violations is 0 against a real, healthy
// ledger, then strictly positive once a posting is corrupted directly (the
// same technique internal/verify's TestRun_BrokenBalanceFails uses: an
// UPDATE, which assert_txn_balanced cannot catch since it only fires AFTER
// INSERT). This is the balance-invariant canary the alert
// LedgerBalanceInvariantViolation (deploy/alerts.yml) watches: it must read
// 0 in steady state and instantly detect the one failure mode that must
// never happen.
func TestCollector_Integration_BalanceInvariantCanary(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	tenant, txnID := postTxn(t, pool)
	t.Cleanup(func() { drainChainer(t, pool, tenant) })

	c := opsmetrics.NewCollector(pool, testLogger())

	// Healthy: the canary must read exactly 0 before any corruption.
	c.Collect(ctx)
	if got := testutil.ToFloat64(metrics.BalanceInvariantViolations); got != 0 {
		t.Fatalf("BalanceInvariantViolations = %v before corruption, want 0", got)
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
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE postings SET amount = amount - 1 WHERE id = $1`, postingID,
		); err != nil {
			t.Errorf("restore corrupted posting: %v", err)
		}
	})

	// Broken: the canary must go strictly positive the moment the invariant
	// is violated, with no restart or cache to clear first.
	c.Collect(ctx)
	if got := testutil.ToFloat64(metrics.BalanceInvariantViolations); got <= 0 {
		t.Fatalf("BalanceInvariantViolations = %v after corrupting a posting, want > 0", got)
	}
}
