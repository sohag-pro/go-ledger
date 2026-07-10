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

// TestMigration0011_BackfillsAndEnforcesFK runs migration 0011 in isolation
// against its own Postgres container, not the package's shared one (this test
// needs to control the exact migration version, rather than start from the
// latest already-migrated schema). It migrates to 0010, seeds an account row
// under two distinct pre-existing tenant ids the way real pre-0011 data would
// (including the well-known default tenant id, mirroring a real deployment),
// migrates forward to 0011, and checks the backfill created one active tenant
// row per referenced id. It then reverses (down to 0010, dropping the foreign
// keys and the table) and re-applies (up to 0011 again), proving the
// migration is cleanly reversible (up, down, up). Finally it proves the new
// foreign keys are real: inserting an account under a tenant id that was
// never backfilled into tenants is rejected.
func TestMigration0011_BackfillsAndEnforcesFK(t *testing.T) {
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

	// Migrate to just before tenants: 0010.
	if err := goose.UpTo(sqlDB, "migrations", 10); err != nil {
		t.Fatalf("migrate to 0010: %v", err)
	}

	const defaultTenant = "00000000-0000-0000-0000-000000000001"
	otherTenant := uuid.NewString()

	mustExecDB(t, sqlDB, `INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1,$2,'A','asset','USD')`,
		uuid.NewString(), defaultTenant)
	mustExecDB(t, sqlDB, `INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1,$2,'B','asset','USD')`,
		uuid.NewString(), otherTenant)

	// Migrate forward through 0011: the backfill must run against this
	// pre-existing data, and the FKs must be added only after it.
	if err := goose.UpTo(sqlDB, "migrations", 11); err != nil {
		t.Fatalf("migrate to 0011: %v", err)
	}
	assertTenantActive(t, sqlDB, defaultTenant)
	assertTenantActive(t, sqlDB, otherTenant)

	// Down: the FKs and the table must both go away cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 10); err != nil {
		t.Fatalf("migrate down to 0010: %v", err)
	}
	var tableExists bool
	if err := sqlDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'tenants')`,
	).Scan(&tableExists); err != nil {
		t.Fatalf("check tenants table exists: %v", err)
	}
	if tableExists {
		t.Error("tenants table still exists after migrating down to 0010")
	}

	// Up again: must re-apply cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 11); err != nil {
		t.Fatalf("migrate up to 0011 again: %v", err)
	}
	assertTenantActive(t, sqlDB, defaultTenant)
	assertTenantActive(t, sqlDB, otherTenant)

	// The new foreign key is real: an account referencing a tenant id with no
	// tenants row is rejected, not silently accepted.
	orphanTenant := uuid.NewString()
	if _, err := sqlDB.Exec(
		`INSERT INTO accounts (id, tenant_id, name, type, currency) VALUES ($1,$2,'C','asset','USD')`,
		uuid.NewString(), orphanTenant,
	); err == nil {
		t.Error("expected an account referencing an unknown tenant id to be rejected by accounts_tenant_fk, got nil")
	}
}

func mustExecDB(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func assertTenantActive(t *testing.T, db *sql.DB, tenantID string) {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status); err != nil {
		t.Fatalf("get tenant %s: %v", tenantID, err)
	}
	if status != "active" {
		t.Errorf("tenant %s status = %q, want active", tenantID, status)
	}
}
