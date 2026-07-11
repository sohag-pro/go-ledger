package postgres_test

// Migration 0031 (ADR-020) makes fx_rates.spread_bps nullable (a per-pair
// override versus "use the applicable markup default") and adds
// fx_markup_defaults, the append-only table those defaults live in. Finding
// 6 of the whole-branch audit flagged the down migration: it used to
// flatten every default-following row (spread_bps IS NULL) to a concrete
// zero before making the column NOT NULL again, discarding whatever markup
// those rows were actually charging at down-migrate time. The fix freezes
// in the current GLOBAL default markup (if any) instead of zero, and reads
// fx_markup_defaults for that value BEFORE dropping it. These tests prove
// both the ordinary reversibility (mirroring migration0030_test.go's shape)
// and the specific backfill behavior finding 6 fixed.
import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// newMigration0031TestDB starts a fresh Postgres container and returns a
// *sql.DB wired for goose, migrated up through 0030 (the state immediately
// before this migration). It skips the test rather than failing it when no
// container can be started, matching every other migration test in this
// package.
func newMigration0031TestDB(t *testing.T) *sql.DB {
	t.Helper()
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
		t.Skipf("skipping integration test: cannot start postgres container (is Docker running?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate to 0030: %v", err)
	}
	return sqlDB
}

// TestMigration0031_Reversible proves migration 0031 is cleanly reversible:
// up makes fx_rates.spread_bps nullable and creates fx_markup_defaults, down
// drops fx_markup_defaults and makes spread_bps NOT NULL again, and up
// re-applies cleanly afterward.
func TestMigration0031_Reversible(t *testing.T) {
	sqlDB := newMigration0031TestDB(t)

	assertSpreadBpsNotNull(t, sqlDB, true)
	assertFXMarkupDefaultsExists(t, sqlDB, false)

	if err := goose.UpTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate to 0031: %v", err)
	}
	assertSpreadBpsNotNull(t, sqlDB, false)
	assertFXMarkupDefaultsExists(t, sqlDB, true)

	if err := goose.DownTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate down to 0030: %v", err)
	}
	assertSpreadBpsNotNull(t, sqlDB, true)
	assertFXMarkupDefaultsExists(t, sqlDB, false)

	if err := goose.UpTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate up to 0031 again: %v", err)
	}
	assertSpreadBpsNotNull(t, sqlDB, false)
	assertFXMarkupDefaultsExists(t, sqlDB, true)
}

// TestMigration0031_DownFreezesGlobalDefaultInsteadOfZero covers finding 6:
// a default-following fx_rates row (spread_bps NULL) must be backfilled with
// whatever the GLOBAL markup default actually was at down-migrate time, not
// flattened to zero. This proves the down migration reads
// fx_markup_defaults (for the current global default) before dropping it,
// and that the read value, not a hardcoded zero, lands in the row.
func TestMigration0031_DownFreezesGlobalDefaultInsteadOfZero(t *testing.T) {
	sqlDB := newMigration0031TestDB(t)
	if err := goose.UpTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate to 0031: %v", err)
	}

	tenantID := insertTestTenant(t, sqlDB, "migration 0031 backfill tenant")

	// A default-following rate row: spread_bps NULL, meaning "whatever the
	// applicable markup default is."
	if _, err := sqlDB.Exec(
		`INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ($1, 'ZZA', 'ZZB', 100000000, NULL, 'test', now())`,
		tenantID,
	); err != nil {
		t.Fatalf("insert default-following fx_rates row: %v", err)
	}

	// The global default markup actually in effect: 77 bps.
	if _, err := sqlDB.Exec(
		`INSERT INTO fx_markup_defaults (tenant_id, default_spread_bps, source, effective_at)
		 VALUES (NULL, 77, 'test', now())`,
	); err != nil {
		t.Fatalf("insert global markup default: %v", err)
	}

	if err := goose.DownTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate down to 0030: %v", err)
	}

	var got int
	if err := sqlDB.QueryRow(
		`SELECT spread_bps FROM fx_rates WHERE tenant_id = $1 AND base = 'ZZA' AND quote = 'ZZB'`,
		tenantID,
	).Scan(&got); err != nil {
		t.Fatalf("read back spread_bps: %v", err)
	}
	if got != 77 {
		t.Errorf("spread_bps after down-migration = %d, want 77 (the global default markup in effect, not flattened to 0)", got)
	}
}

// TestMigration0031_DownFallsBackToZeroWhenNoGlobalDefault covers the other
// half of finding 6's fix: when NO global markup default was ever set, a
// default-following row has nothing to freeze in, so it must fall back to
// the pre-ADR-020 behavior of a concrete zero (COALESCE's other branch),
// not fail the down migration or leave the column impossible to make NOT
// NULL.
func TestMigration0031_DownFallsBackToZeroWhenNoGlobalDefault(t *testing.T) {
	sqlDB := newMigration0031TestDB(t)
	if err := goose.UpTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate to 0031: %v", err)
	}

	tenantID := insertTestTenant(t, sqlDB, "migration 0031 no-default tenant")

	if _, err := sqlDB.Exec(
		`INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ($1, 'ZZC', 'ZZD', 100000000, NULL, 'test', now())`,
		tenantID,
	); err != nil {
		t.Fatalf("insert default-following fx_rates row: %v", err)
	}
	// Deliberately no fx_markup_defaults row at all.

	if err := goose.DownTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate down to 0030: %v", err)
	}

	var got int
	if err := sqlDB.QueryRow(
		`SELECT spread_bps FROM fx_rates WHERE tenant_id = $1 AND base = 'ZZC' AND quote = 'ZZD'`,
		tenantID,
	).Scan(&got); err != nil {
		t.Fatalf("read back spread_bps: %v", err)
	}
	if got != 0 {
		t.Errorf("spread_bps after down-migration with no global default = %d, want 0", got)
	}
}

// TestMigration0031_DownMapsClearedGlobalDefaultToZero covers the residual
// finding in finding 6's own fix: the backfill subquery used to add
// "AND default_spread_bps IS NOT NULL", which skips a CLEARED latest global
// row (a NULL value, meaning "no markup, i.e. 0") and falls through to
// whatever OLDER, superseded non-null value came before it, freezing that
// stale value into the down-migrated rows instead of 0. A global default that
// was explicitly cleared before down-migrate time must freeze to 0, exactly
// like "no global default was ever set" (the sibling test above), not to
// whatever it used to be before the clear.
func TestMigration0031_DownMapsClearedGlobalDefaultToZero(t *testing.T) {
	sqlDB := newMigration0031TestDB(t)
	if err := goose.UpTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate to 0031: %v", err)
	}

	tenantID := insertTestTenant(t, sqlDB, "migration 0031 cleared-default tenant")

	if _, err := sqlDB.Exec(
		`INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ($1, 'ZZE', 'ZZF', 100000000, NULL, 'test', now())`,
		tenantID,
	); err != nil {
		t.Fatalf("insert default-following fx_rates row: %v", err)
	}

	// An older global default of 77 bps, then a later CLEAR (NULL): the
	// latest global row in effect at down-migrate time is the clear, not 77.
	if _, err := sqlDB.Exec(
		`INSERT INTO fx_markup_defaults (tenant_id, default_spread_bps, source, effective_at)
		 VALUES (NULL, 77, 'test', now() - interval '1 hour')`,
	); err != nil {
		t.Fatalf("insert older global markup default: %v", err)
	}
	if _, err := sqlDB.Exec(
		`INSERT INTO fx_markup_defaults (tenant_id, default_spread_bps, source, effective_at)
		 VALUES (NULL, NULL, 'test', now())`,
	); err != nil {
		t.Fatalf("insert cleared global markup default: %v", err)
	}

	if err := goose.DownTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate down to 0030: %v", err)
	}

	var got int
	if err := sqlDB.QueryRow(
		`SELECT spread_bps FROM fx_rates WHERE tenant_id = $1 AND base = 'ZZE' AND quote = 'ZZF'`,
		tenantID,
	).Scan(&got); err != nil {
		t.Fatalf("read back spread_bps: %v", err)
	}
	if got != 0 {
		t.Errorf("spread_bps after down-migration with a cleared latest global default = %d, want 0 (not the older superseded 77)", got)
	}
}

// insertTestTenant inserts a minimal tenants row directly (these tests work
// at the raw *sql.DB/goose level, not through postgres.Repository), and
// returns its id, so a scoped fx_rates row satisfies the tenant_id foreign
// key.
func insertTestTenant(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`INSERT INTO tenants (id, name) VALUES (gen_random_uuid(), $1) RETURNING id::text`, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return id
}

// assertSpreadBpsNotNull fails t unless fx_rates.spread_bps's NOT NULL-ness
// matches want.
func assertSpreadBpsNotNull(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT attnotnull FROM pg_attribute
		 WHERE attrelid = 'fx_rates'::regclass AND attname = 'spread_bps'`,
	).Scan(&got); err != nil {
		t.Fatalf("check fx_rates.spread_bps attnotnull: %v", err)
	}
	if got != want {
		t.Errorf("fx_rates.spread_bps NOT NULL = %v, want %v", got, want)
	}
}

// assertFXMarkupDefaultsExists fails t unless fx_markup_defaults's existence
// matches want.
func assertFXMarkupDefaultsExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'fx_markup_defaults')`,
	).Scan(&got); err != nil {
		t.Fatalf("check fx_markup_defaults existence: %v", err)
	}
	if got != want {
		t.Errorf("fx_markup_defaults exists = %v, want %v", got, want)
	}
}
