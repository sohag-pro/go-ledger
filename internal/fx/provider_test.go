package fx_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

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
		SpreadBps:   spreadBps,
		Source:      "test",
		EffectiveAt: effectiveAt,
	}); err != nil {
		t.Fatalf("insert fx rate %s/%s: %v", base, quote, err)
	}
}

// TestRate_Direct covers the plain case: the pair is stored exactly as
// requested, so Rate returns its mid and spread unchanged.
func TestRate_Direct(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	insertRate(t, q, "GBP", "CHF", 115_000_000, 30, time.Now().UTC())

	provider := fx.NewDBProvider(pool)
	quote, spreadBps, err := provider.Rate(ctx, domain.Currency("GBP"), domain.Currency("CHF"))
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
	insertRate(t, q, "JPY", "CAD", midE8, 40, time.Now().UTC())

	provider := fx.NewDBProvider(pool)
	quote, spreadBps, err := provider.Rate(ctx, domain.Currency("CAD"), domain.Currency("JPY"))
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
	_, _, err := provider.Rate(ctx, domain.Currency("NZD"), domain.Currency("SEK"))
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

	effectiveAt := time.Now().UTC()
	insertRate(t, q, "AUD", "NOK", 100_000_000, 10, effectiveAt)
	insertRate(t, q, "AUD", "NOK", 200_000_000, 20, effectiveAt)

	row, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "AUD", Quote: "NOK"})
	if err != nil {
		t.Fatalf("CurrentFXRate() error = %v", err)
	}
	if row.MidRateE8 != 200_000_000 || row.SpreadBps != 20 {
		t.Errorf("CurrentFXRate() = {mid: %d, spread: %d}, want the later-inserted row {mid: 200000000, spread: 20} "+
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
