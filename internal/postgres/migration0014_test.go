package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestMigration0014_PerTenantFXRates runs migration 0014 in isolation (Task
// 2.4, audit A3.3), the same up/down/up pattern TestMigration0011,
// TestMigration0012, and TestMigration0013_FingerprintSchemeDefaultsToV1 use:
// it migrates to 0013, inserts a global fx_rates row the way every row
// before this migration looks (no tenant_id column yet), migrates forward to
// 0014, and checks the pre-existing row picked up tenant_id NULL (the global
// default). It then inserts a tenant-scoped row to prove the column accepts
// a real tenants.id, reverses (down to 0013, dropping the column and index),
// and re-applies (up to 0014 again), proving the migration is cleanly
// reversible and that the pre-existing global row still resolves as global
// afterward.
func TestMigration0014_PerTenantFXRates(t *testing.T) {
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
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	// Migrate to just before tenant_id: 0013.
	if err := goose.UpTo(sqlDB, "migrations", 13); err != nil {
		t.Fatalf("migrate to 0013: %v", err)
	}

	// A pre-existing row, written the way every row before this migration
	// looks: no tenant_id column exists at this point.
	mustExecDB(t, sqlDB,
		`INSERT INTO fx_rates (base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ('USD', 'PLN', 400000000, 15, 'pre-2.4', now())`)

	// Migrate forward through 0014: the pre-existing row must resolve as the
	// global default (tenant_id IS NULL).
	if err := goose.UpTo(sqlDB, "migrations", 14); err != nil {
		t.Fatalf("migrate to 0014: %v", err)
	}
	assertFXRateTenantID(t, sqlDB, "USD", "PLN", "pre-2.4", nil)

	// A tenant-scoped row must accept a real tenants.id (the new foreign
	// key), proving the column is wired to the right table, not just typed
	// uuid.
	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0014 test tenant')`, tenant)
	mustExecDB(t, sqlDB,
		`INSERT INTO fx_rates (tenant_id, base, quote, mid_rate_e8, spread_bps, source, effective_at)
		 VALUES ($1, 'USD', 'PLN', 410000000, 60, 'tenant-scoped', now())`,
		tenant)
	assertFXRateTenantID(t, sqlDB, "USD", "PLN", "tenant-scoped", &tenant)

	// Down: the column (and its index) must go away cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 13); err != nil {
		t.Fatalf("migrate down to 0013: %v", err)
	}
	var columnExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'fx_rates' AND column_name = 'tenant_id')`,
	).Scan(&columnExists); err != nil {
		t.Fatalf("check column fx_rates.tenant_id exists: %v", err)
	}
	if columnExists {
		t.Error("fx_rates.tenant_id still exists after migrating down to 0013")
	}

	// Up again: must re-apply cleanly, and the pre-existing global row must
	// still resolve as the global default.
	if err := goose.UpTo(sqlDB, "migrations", 14); err != nil {
		t.Fatalf("migrate up to 0014 again: %v", err)
	}
	assertFXRateTenantID(t, sqlDB, "USD", "PLN", "pre-2.4", nil)
}

// assertFXRateTenantID checks the tenant_id of the fx_rates row identified by
// (base, quote, source): nil wantTenant means the row must be the global
// default (tenant_id NULL); a non-nil wantTenant means the row must carry
// exactly that tenant id.
func assertFXRateTenantID(t *testing.T, db *sql.DB, base, quote, source string, wantTenant *string) {
	t.Helper()
	var gotTenant sql.NullString
	if err := db.QueryRow(
		`SELECT tenant_id::text FROM fx_rates WHERE base = $1 AND quote = $2 AND source = $3`,
		base, quote, source,
	).Scan(&gotTenant); err != nil {
		t.Fatalf("get tenant_id for %s/%s (%s): %v", base, quote, source, err)
	}
	switch {
	case wantTenant == nil && gotTenant.Valid:
		t.Errorf("fx_rates %s/%s (%s) tenant_id = %q, want NULL (global default)", base, quote, source, gotTenant.String)
	case wantTenant != nil && (!gotTenant.Valid || gotTenant.String != *wantTenant):
		t.Errorf("fx_rates %s/%s (%s) tenant_id = %v, want %q", base, quote, source, gotTenant, *wantTenant)
	}
}
