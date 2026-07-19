package fx_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// One Postgres container is shared across the whole package, started once in
// TestMain, mirroring internal/postgres's own test setup. fx_rates has no
// tenant_id column (it is global configuration, not per-tenant data), so
// tests isolate themselves by using a disjoint currency pair each rather than
// a tenant id.
var (
	sharedPool *pgxpool.Pool
	poolErr    error

	// noDefaultSpreadBps and noDefaultErr capture Provider.Rate's resolved
	// spread for a disjoint, freshly seeded rate row, taken once in
	// runWithContainer right after migrations and before m.Run() starts any
	// test. fx_markup_defaults is shared and append-only across the whole
	// package, with no per-currency-pair partition (GlobalFXMarkupDefault
	// matches whatever the most recently inserted global default is, for
	// every pair and tenant), so "nothing configured anywhere" can only be
	// observed live once per process, at this exact moment; every later test
	// (including any that legitimately write a global markup default, such
	// as TestAdminServiceInsertAndList in admin_test.go) makes it
	// unobservable for the rest of the run. TestProviderResolvesMarkupPrecedence
	// asserts against these captured values instead of performing its own
	// live check.
	noDefaultSpreadBps int32
	noDefaultErr       error

	// globalMarkupMu serialises every test window that writes a global
	// (tenant_id NULL) row in fx_markup_defaults and then reads it back,
	// directly or via Provider.Rate / AdminService.ListRates. The row is a
	// single package-wide slot with no per-pair partition, and the resolver
	// picks the most recently inserted one, so two parallel tests that both
	// touch it will race: whichever landed second wins the read the other
	// test was about to assert against. Hold the mutex around the whole
	// "insert-then-observe" pair, not just the insert.
	globalMarkupMu sync.Mutex

	// globalUSDEURRateMu serialises writes to the global (tenant_id NULL)
	// USD/EUR row in fx_rates that several parallel tests share:
	// TestSeed_ParsesExactValues writes it with spread 25 via fx.Seed and
	// reads back the exact value; TestFeed_RefreshWritesAndDedups writes it
	// with NULL spread via the live-feed HTTP mock. CurrentFXRate picks the
	// latest global row for (base, quote), so if the feed writes land
	// between seed's write and its assertion, seed sees a NULL spread
	// instead of 25. Every test that writes USD/EUR globally must acquire
	// this before its write and hold it across the assert.
	globalUSDEURRateMu sync.Mutex
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
		// Wait on the readiness log, not just the open port: Postgres opens 5432
		// during initdb and then restarts it, so a port-only wait races real
		// readiness. The startup log appears twice (initdb, then the real
		// server), hence WithOccurrence(2).
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		// No Docker (or it failed): record it so tests skip rather than fail.
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
	noDefaultSpreadBps, noDefaultErr = probeNoDefaultAnywhere(ctx, pool)
	return m.Run()
}

// probeNoDefaultAnywhere resolves the spread for a disjoint, freshly seeded
// rate row via the real Provider, before any test has had a chance to write
// to fx_markup_defaults (see the noDefaultSpreadBps doc comment above for
// why this must happen here rather than in a live test assertion).
func probeNoDefaultAnywhere(ctx context.Context, pool *pgxpool.Pool) (int32, error) {
	q := sqlc.New(pool)
	if _, err := q.InsertFXRate(ctx, sqlc.InsertFXRateParams{
		Base: "ZZP", Quote: "ZZQ", MidRateE8: 100_000_000,
		Source:      "test",
		EffectiveAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(-2 * time.Second), Valid: true},
	}); err != nil {
		return 0, fmt.Errorf("probe insert fx rate: %w", err)
	}
	_, spreadBps, err := fx.NewDBProvider(pool).Rate(ctx, uuid.NewString(), "ZZP", "ZZQ")
	return spreadBps, err
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

// newTestPool returns the shared pool, skipping the test when no container was
// available (for example no Docker), so the suite stays green without Docker.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	return sharedPool
}

// insertRate writes one fx_rates row directly via sqlc, bypassing Seed, so
// provider tests can set up fixture rows without going through parsing.
func insertRate(t *testing.T, q *sqlc.Queries, base, quote string, midE8 int64, spreadBps int32, effectiveAt time.Time) {
	t.Helper()
	if _, err := q.InsertFXRate(context.Background(), sqlc.InsertFXRateParams{
		Base:        base,
		Quote:       quote,
		MidRateE8:   midE8,
		SpreadBps:   pgtype.Int4{Int32: spreadBps, Valid: true},
		Source:      "test",
		EffectiveAt: pgtype.Timestamptz{Time: effectiveAt, Valid: true},
	}); err != nil {
		t.Fatalf("insert fx rate %s/%s: %v", base, quote, err)
	}
}

// insertTenantRate writes one tenant-scoped fx_rates row directly via sqlc
// (Task 2.4, audit A3.3): tenantID must be an existing tenants.id (see
// newTestTenant), since fx_rates.tenant_id carries a foreign key.
func insertTenantRate(t *testing.T, q *sqlc.Queries, tenantID, base, quote string, midE8 int64, spreadBps int32, effectiveAt time.Time) {
	t.Helper()
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		t.Fatalf("parse tenant id %q: %v", tenantID, err)
	}
	if _, err := q.InsertFXRate(context.Background(), sqlc.InsertFXRateParams{
		TenantID:    pgtype.UUID{Bytes: tid, Valid: true},
		Base:        base,
		Quote:       quote,
		MidRateE8:   midE8,
		SpreadBps:   pgtype.Int4{Int32: spreadBps, Valid: true},
		Source:      "test",
		EffectiveAt: pgtype.Timestamptz{Time: effectiveAt, Valid: true},
	}); err != nil {
		t.Fatalf("insert tenant fx rate %s/%s for tenant %s: %v", base, quote, tenantID, err)
	}
}

// newTestTenant creates and returns a fresh tenant id, so a tenant-scoped
// fx_rates row (fx_rates.tenant_id references tenants.id) has a real row to
// point at.
func newTestTenant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := uuid.NewString()
	if err := postgres.NewRepository(pool).CreateTenant(context.Background(), id, "fx provider test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

// TestRate_Direct covers the plain case: the pair is stored exactly as
// requested, so Rate returns its mid and spread unchanged.
func TestRate_Direct(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	insertRate(t, q, "GBP", "CHF", 115_000_000, 30, time.Now().UTC().Add(-2*time.Second))

	// Rate() requires a tenant id even though this pair has no tenant-specific
	// row: any well-formed id resolves the global default here, since (tenant_id
	// = $1 OR tenant_id IS NULL) always matches a NULL row (see TestRate_TenantOverridesGlobal
	// for the case where a tenant-specific row actually wins).
	provider := fx.NewDBProvider(pool)
	quote, spreadBps, err := provider.Rate(ctx, uuid.NewString(), domain.Currency("GBP"), domain.Currency("CHF"))
	if err != nil {
		t.Fatalf("Rate() error = %v", err)
	}
	if quote.Base != "GBP" || quote.Quote != "CHF" {
		t.Errorf("Rate() base/quote = %s/%s, want GBP/CHF", quote.Base, quote.Quote)
	}
	if quote.MidRateE8 != 115_000_000 {
		t.Errorf("Rate() MidRateE8 = %d, want 115000000", quote.MidRateE8)
	}
	if spreadBps != 30 {
		t.Errorf("Rate() spreadBps = %d, want 30", spreadBps)
	}
}

// TestRate_Inverse covers the inverse rule: only JPY/CAD is stored, so
// Rate(CAD, JPY) must invert the stored mid (RateScale^2 / midE8) and carry
// over the same spread found on the JPY/CAD row.
func TestRate_Inverse(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	const midE8 = 150_000_000 // 1 JPY = 1.5 CAD scaled, an arbitrary fixture rate
	insertRate(t, q, "JPY", "CAD", midE8, 40, time.Now().UTC().Add(-2*time.Second))

	provider := fx.NewDBProvider(pool)
	quote, spreadBps, err := provider.Rate(ctx, uuid.NewString(), domain.Currency("CAD"), domain.Currency("JPY"))
	if err != nil {
		t.Fatalf("Rate() error = %v", err)
	}
	if quote.Base != "CAD" || quote.Quote != "JPY" {
		t.Errorf("Rate() base/quote = %s/%s, want CAD/JPY", quote.Base, quote.Quote)
	}
	wantInverted := (domain.RateScale * domain.RateScale) / int64(midE8)
	if quote.MidRateE8 != wantInverted {
		t.Errorf("Rate() MidRateE8 = %d, want inverted %d", quote.MidRateE8, wantInverted)
	}
	if spreadBps != 40 {
		t.Errorf("Rate() spreadBps = %d, want 40 (the JPY/CAD row's spread, carried through the inversion)", spreadBps)
	}
}

// TestRate_NotFound covers a pair with no row in either direction: Rate must
// return an error wrapping domain.ErrFXRateNotFound, not a zero quote.
func TestRate_NotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()

	provider := fx.NewDBProvider(pool)
	_, _, err := provider.Rate(ctx, uuid.NewString(), domain.Currency("NZD"), domain.Currency("SEK"))
	if !errors.Is(err, domain.ErrFXRateNotFound) {
		t.Fatalf("Rate() error = %v, want domain.ErrFXRateNotFound", err)
	}
}

// TestCurrentFXRate_TiebreakAndAppend gives the sqlc fx_rates queries their
// first real coverage against a live database (Task 4 added them with no
// tests). It proves two things migration 0010 documents but nothing had
// exercised yet:
//  1. InsertFXRate never overwrites: two inserts for the same pair produce
//     two rows, because fx_rates is append-only.
//  2. CurrentFXRate's ORDER BY effective_at DESC, id DESC resolves two rows
//     that share the exact same effective_at (a re-seed within the same
//     second) to the later-inserted (higher id) row, deterministically.
func TestCurrentFXRate_TiebreakAndAppend(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	effectiveAt := time.Now().UTC().Add(-2 * time.Second)
	insertRate(t, q, "AUD", "NOK", 100_000_000, 10, effectiveAt)
	insertRate(t, q, "AUD", "NOK", 200_000_000, 20, effectiveAt)

	row, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "AUD", Quote: "NOK"})
	if err != nil {
		t.Fatalf("CurrentFXRate() error = %v", err)
	}
	if row.MidRateE8 != 200_000_000 || !row.SpreadBps.Valid || row.SpreadBps.Int32 != 20 {
		t.Errorf("CurrentFXRate() = {mid: %d, spread: %v}, want the later-inserted row {mid: 200000000, spread: 20} "+
			"(effective_at tie should break on id DESC)", row.MidRateE8, row.SpreadBps)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM fx_rates WHERE base = $1 AND quote = $2`, "AUD", "NOK").
		Scan(&count); err != nil {
		t.Fatalf("count fx_rates rows: %v", err)
	}
	if count != 2 {
		t.Errorf("fx_rates row count for AUD/NOK = %d, want 2 (append, never overwrite)", count)
	}
}

// TestRate_TenantOverridesGlobal is the discriminating test for Task 2.4
// (audit A3.3): with only a global SGD/HKD row, both tenants resolve it; once
// tenant A gets its own SGD/HKD row with a different mid and spread, tenant A
// resolves its own row while tenant B still resolves the global one, and the
// inverse pair (HKD/SGD) honors the same per-tenant preference.
func TestRate_TenantOverridesGlobal(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	provider := fx.NewDBProvider(pool)

	tenantA := newTestTenant(t, pool)
	tenantB := newTestTenant(t, pool)

	const (
		globalMidE8  = 55_000_000
		globalSpread = 20
		tenantMidE8  = 60_000_000
		tenantSpread = 75
	)
	const (
		base  domain.Currency = "SGD"
		quote domain.Currency = "HKD"
	)
	insertRate(t, q, string(base), string(quote), globalMidE8, globalSpread, time.Now().UTC().Add(-2*time.Second))

	// Before tenant A has a row of its own, both tenants resolve the global
	// default.
	for _, tenant := range []string{tenantA, tenantB} {
		got, spreadBps, err := provider.Rate(ctx, tenant, base, quote)
		if err != nil {
			t.Fatalf("Rate(%s) error = %v", tenant, err)
		}
		if got.MidRateE8 != globalMidE8 || spreadBps != globalSpread {
			t.Errorf("Rate(%s) = {mid: %d, spread: %d}, want the global row {mid: %d, spread: %d}",
				tenant, got.MidRateE8, spreadBps, globalMidE8, globalSpread)
		}
	}

	insertTenantRate(t, q, tenantA, string(base), string(quote), tenantMidE8, tenantSpread, time.Now().UTC().Add(-2*time.Second))

	// Tenant A now resolves its own row.
	gotA, spreadA, err := provider.Rate(ctx, tenantA, base, quote)
	if err != nil {
		t.Fatalf("Rate(tenantA) error = %v", err)
	}
	if gotA.MidRateE8 != tenantMidE8 || spreadA != tenantSpread {
		t.Errorf("Rate(tenantA) = {mid: %d, spread: %d}, want the tenant row {mid: %d, spread: %d}",
			gotA.MidRateE8, spreadA, tenantMidE8, tenantSpread)
	}

	// Tenant B, with no row of its own, still resolves the global default.
	gotB, spreadB, err := provider.Rate(ctx, tenantB, base, quote)
	if err != nil {
		t.Fatalf("Rate(tenantB) error = %v", err)
	}
	if gotB.MidRateE8 != globalMidE8 || spreadB != globalSpread {
		t.Errorf("Rate(tenantB) = {mid: %d, spread: %d}, want the global row {mid: %d, spread: %d}",
			gotB.MidRateE8, spreadB, globalMidE8, globalSpread)
	}

	// The inverse pair (HKD/SGD) must honor the same per-tenant preference:
	// tenant A inverts its own row, tenant B inverts the global one.
	wantInvertedA := (domain.RateScale * domain.RateScale) / int64(tenantMidE8)
	invA, invSpreadA, err := provider.Rate(ctx, tenantA, quote, base)
	if err != nil {
		t.Fatalf("Rate(tenantA, inverse) error = %v", err)
	}
	if invA.MidRateE8 != wantInvertedA || invSpreadA != tenantSpread {
		t.Errorf("Rate(tenantA, inverse) = {mid: %d, spread: %d}, want {mid: %d, spread: %d} (inverted tenant row)",
			invA.MidRateE8, invSpreadA, wantInvertedA, tenantSpread)
	}

	wantInvertedGlobal := (domain.RateScale * domain.RateScale) / int64(globalMidE8)
	invB, invSpreadB, err := provider.Rate(ctx, tenantB, quote, base)
	if err != nil {
		t.Fatalf("Rate(tenantB, inverse) error = %v", err)
	}
	if invB.MidRateE8 != wantInvertedGlobal || invSpreadB != globalSpread {
		t.Errorf("Rate(tenantB, inverse) = {mid: %d, spread: %d}, want {mid: %d, spread: %d} (inverted global row)",
			invB.MidRateE8, invSpreadB, wantInvertedGlobal, globalSpread)
	}
}
