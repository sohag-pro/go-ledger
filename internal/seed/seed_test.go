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

	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/seed"
)

var (
	sharedPool *pgxpool.Pool
	poolErr    error
)

// testDemoKeyHash stands in for domain.HashAPIKey(cfg.demoAPIKey) in tests
// that do not care about the actual demo key value, only that Seed is given
// some hash to compare a tenant's api_keys rows against.
const testDemoKeyHash = "test-demo-key-hash-not-a-real-hash"

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

	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Seven accounts (four USD plus blank EUR, BDT, MYR).
	accts, err := repo.ListAccounts(ctx, tenant.String(), 100)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accts) != 7 {
		t.Fatalf("got %d accounts, want 7", len(accts))
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
	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if after := countTxns(t, pool, tenant); after != before {
		t.Errorf("re-seed changed transaction count: %d then %d (expected reset, not append)", before, after)
	}
}

// TestSeedResetsAuditAndIdempotency proves the reset clears idempotency_keys,
// audit_log, and audit_outbox (ADR-017) for the tenant, and that it does so
// despite audit_log's append-only immutability trigger (via the seeder's
// gated SET LOCAL). The seeder itself writes raw rows and never populates
// these tables, so this test stands in for the application path: it attaches
// a fabricated idempotency key, audit row, and outbox row to one of the
// seeded transactions, then re-seeds and checks all three are gone. The
// outbox row matters here beyond "is it cleared": audit_outbox.transaction_id
// references transactions(id) (migration 0015), the same as idempotency_keys
// and audit_log already do, so a reset that deleted transactions before
// audit_outbox would fail a foreign-key check with a still-referencing
// outbox row in place; this test would catch that ordering regression.
func TestSeedResetsAuditAndIdempotency(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	now := time.Now()

	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var txnID uuid.UUID
	if err := pool.QueryRow(ctx,
		"SELECT id FROM transactions WHERE tenant_id = $1 LIMIT 1", tenant).Scan(&txnID); err != nil {
		t.Fatalf("find seeded transaction: %v", err)
	}

	// expires_at (Task 4.5, audit A1.4) is NOT NULL as of migration 0019, so
	// this fabricated row must supply one, the same as the real application
	// insert path does; a comfortably future value keeps it live for the
	// rest of this test regardless of how long the reset takes.
	if _, err := pool.Exec(ctx,
		`INSERT INTO idempotency_keys (tenant_id, idempotency_key, fingerprint, transaction_id, expires_at)
		 VALUES ($1, 'test-key', 'test-fingerprint', $2, now() + interval '1 hour')`, tenant, txnID); err != nil {
		t.Fatalf("insert idempotency key: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_log (id, tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, $2, 'transaction.created', $3, $4, '{}'::jsonb)`,
		uuid.New(), tenant, txnID, tenant.String()); err != nil {
		t.Fatalf("insert audit row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, after)
		 VALUES ($1, 'transaction.created', $2, $3, '{}'::jsonb)`,
		tenant, txnID, tenant.String()); err != nil {
		t.Fatalf("insert audit outbox row: %v", err)
	}

	// Re-seeding must clear all three, even though audit_log rejects DELETE
	// outside the seeder's gated transaction, and even though audit_outbox
	// (like idempotency_keys and audit_log) references the transactions row
	// this reset is about to delete.
	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("re-seed over idempotency, audit, and outbox rows: %v", err)
	}

	var idemCount, auditCount, outboxCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1", tenant).Scan(&idemCount); err != nil {
		t.Fatalf("count idempotency_keys: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_log WHERE tenant_id = $1", tenant).Scan(&auditCount); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM audit_outbox WHERE tenant_id = $1", tenant).Scan(&outboxCount); err != nil {
		t.Fatalf("count audit_outbox: %v", err)
	}
	if idemCount != 0 {
		t.Errorf("idempotency_keys not cleared on reset: %d rows remain", idemCount)
	}
	if auditCount != 0 {
		t.Errorf("audit_log not cleared on reset: %d rows remain", auditCount)
	}
	if outboxCount != 0 {
		t.Errorf("audit_outbox not cleared on reset: %d rows remain", outboxCount)
	}
}

// TestSeedClearsAPISourcedGlobalFXConfig proves the reset removes a global
// (tenant_id NULL) fx_rates or fx_markup_defaults row written through the
// admin API (source 'api'), the fix for the finding that a demo visitor could
// POST a global markup of 9999 bps or a garbage global mid rate through the
// public FX admin endpoints and have it survive every reset, mispricing
// every other visitor's conversions. tenant_id NULL rows are process-wide,
// not owned by any one tenant, so the tenant-scoped delete loop above never
// reaches them: this is the only thing that does. An env-sourced global row
// (source 'env', the shape fx.Seed writes from FX_RATES at boot) must
// survive untouched, so the demo's configured rates still apply right after
// a reset.
//
// It also proves two more scopes the reset must get right, since CurrentFXRate
// prefers a tenant-owned fx_rates row over the global one: an api-sourced
// fx_rates row owned by the demo tenant itself (not global, and not in the
// tenant-scoped delete loop above either, since fx_rates is not one of the
// tables that loop clears) must also be gone after reset, or a visitor could
// POST a garbage rate scoped to the demo tenant id and have it survive every
// reset and mis-price every demo conversion. And an api-sourced
// fx_markup_defaults row belonging to a DIFFERENT, unrelated tenant must
// survive: the markup delete clears source='api' rows globally, and scoping
// it to "global or demo tenant" must not widen to "every tenant."
func TestSeedClearsAPISourcedGlobalFXConfig(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	otherTenant := uuid.New()
	now := time.Now()

	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// otherTenant is a stand-in for an unrelated, live (non-demo) tenant: its
	// own api-sourced markup default must not be touched by the demo reset.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, 'other-tenant') ON CONFLICT (id) DO NOTHING`,
		otherTenant); err != nil {
		t.Fatalf("insert other tenant: %v", err)
	}

	// A tampered global rate and markup, as an anonymous demo visitor could
	// post through the unauthenticated admin endpoints in demo mode.
	if _, err := pool.Exec(ctx,
		`INSERT INTO fx_rates (base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ('USD', 'EUR', 1, 9999, 'api', now())`); err != nil {
		t.Fatalf("insert tampered api fx rate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO fx_markup_defaults (default_spread_bps, source, effective_at)
		 VALUES (9999, 'api', now())`); err != nil {
		t.Fatalf("insert tampered api markup default: %v", err)
	}
	// A tampered rate scoped to the demo TENANT itself, as a visitor could
	// post by supplying the demo tenant id in the request body. CurrentFXRate
	// prefers this over the global row, so it must not survive a reset either.
	if _, err := pool.Exec(ctx,
		`INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ($1, 'USD', 'CAD', 1, 9999, 'api', now())`, tenant); err != nil {
		t.Fatalf("insert tampered tenant-scoped api fx rate: %v", err)
	}
	// A legitimate api-sourced markup default belonging to some other,
	// unrelated tenant: not global, not the demo tenant, so the reset must
	// leave it alone.
	if _, err := pool.Exec(ctx,
		`INSERT INTO fx_markup_defaults (tenant_id, default_spread_bps, source, effective_at)
		 VALUES ($1, 75, 'api', now())`, otherTenant); err != nil {
		t.Fatalf("insert other tenant api markup default: %v", err)
	}
	// A legitimate env-seeded global rate and markup, standing in for what
	// fx.Seed writes from FX_RATES at process boot: this must survive.
	if _, err := pool.Exec(ctx,
		`INSERT INTO fx_rates (base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ('USD', 'GBP', 92000000, 25, 'env', now())`); err != nil {
		t.Fatalf("insert env fx rate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO fx_markup_defaults (default_spread_bps, source, effective_at)
		 VALUES (50, 'env', now())`); err != nil {
		t.Fatalf("insert env markup default: %v", err)
	}

	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("re-seed over tampered global fx config: %v", err)
	}

	var apiRateCount, tenantAPIRateCount, envRateCount, apiMarkupCount, otherTenantMarkupCount, envMarkupCount int
	// tenant_id IS NULL isolates the tampered GLOBAL USD/EUR row this checks;
	// the demo prefill legitimately writes a tenant-scoped USD/EUR api row, so
	// an unscoped count would also pick that up.
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM fx_rates WHERE base = 'USD' AND quote = 'EUR' AND source = 'api' AND tenant_id IS NULL").Scan(&apiRateCount); err != nil {
		t.Fatalf("count api fx rates: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM fx_rates WHERE base = 'USD' AND quote = 'CAD' AND tenant_id = $1 AND source = 'api'",
		tenant).Scan(&tenantAPIRateCount); err != nil {
		t.Fatalf("count tenant-scoped api fx rates: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM fx_rates WHERE base = 'USD' AND quote = 'GBP' AND source = 'env'").Scan(&envRateCount); err != nil {
		t.Fatalf("count env fx rates: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM fx_markup_defaults WHERE default_spread_bps = 9999 AND source = 'api'").Scan(&apiMarkupCount); err != nil {
		t.Fatalf("count api markup defaults: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM fx_markup_defaults WHERE tenant_id = $1 AND default_spread_bps = 75 AND source = 'api'",
		otherTenant).Scan(&otherTenantMarkupCount); err != nil {
		t.Fatalf("count other-tenant api markup defaults: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM fx_markup_defaults WHERE default_spread_bps = 50 AND source = 'env'").Scan(&envMarkupCount); err != nil {
		t.Fatalf("count env markup defaults: %v", err)
	}

	if apiRateCount != 0 {
		t.Errorf("api-sourced global fx rate survived reset: %d rows remain, want 0", apiRateCount)
	}
	if tenantAPIRateCount != 0 {
		t.Errorf("api-sourced demo-tenant-scoped fx rate survived reset: %d rows remain, want 0", tenantAPIRateCount)
	}
	if envRateCount != 1 {
		t.Errorf("env-sourced global fx rate did not survive reset: got %d rows, want 1", envRateCount)
	}
	if apiMarkupCount != 0 {
		t.Errorf("api-sourced global fx markup default survived reset: %d rows remain, want 0", apiMarkupCount)
	}
	if otherTenantMarkupCount != 1 {
		t.Errorf("other tenant's api-sourced fx markup default did not survive reset: got %d rows, want 1", otherTenantMarkupCount)
	}
	if envMarkupCount != 1 {
		t.Errorf("env-sourced global fx markup default did not survive reset: got %d rows, want 1", envMarkupCount)
	}
}

// TestSeedPrefillsDemoTenantFX proves the demo seeder prefills the demo tenant
// with the starter USD-based FX rates and a 1 percent markup, so the console's
// Exchange rates page is not empty right after a reset.
func TestSeedPrefillsDemoTenantFX(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.New()
	now := time.Now()

	if err := seed.Seed(ctx, pool, tenant.String(), now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	svc := fx.NewAdminService(pool)
	rates, err := svc.ListRates(ctx, tenant.String())
	if err != nil {
		t.Fatalf("ListRates: %v", err)
	}
	have := map[string]bool{}
	for _, r := range rates {
		have[r.Base+"/"+r.Quote] = true
	}
	for _, quote := range []string{"EUR", "MYR", "BDT", "INR"} {
		if !have["USD/"+quote] {
			t.Errorf("demo tenant missing prefilled USD/%s rate after seed", quote)
		}
	}

	mv, err := svc.GetMarkup(ctx, tenant.String())
	if err != nil {
		t.Fatalf("GetMarkup: %v", err)
	}
	if mv.Tenant == nil || mv.Tenant.DefaultSpreadBps == nil || *mv.Tenant.DefaultSpreadBps != 100 {
		t.Fatalf("demo tenant markup = %+v, want a 100 bps default", mv.Tenant)
	}
}
