package postgres_test

// Migration 0025 (Task 5.3, audit A2.4) adds audit_anchors: the off-box
// anchoring table for the tamper-evident audit chain. It follows the exact
// same RLS shape migration 0024 established (ENABLE + FORCE + one
// allow-when-unset tenant_isolation policy), so these tests mirror
// migration0024_test.go's own reversibility and RLS-behavior tests, scoped
// to just this one new table.

import (
	"context"
	"database/sql"
	"fmt"
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

// TestMigration0025_Reversible proves migration 0025 is cleanly reversible:
// up creates audit_anchors with exactly one tenant_isolation policy and
// FORCE set, down actually removes all of that (not merely "no error"), and
// up re-applies cleanly afterward. Same up/down/up shape as
// TestMigration0024_Reversible.
func TestMigration0025_Reversible(t *testing.T) {
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

	// Up through 0024: no audit_anchors table yet.
	if err := goose.UpTo(sqlDB, "migrations", 24); err != nil {
		t.Fatalf("migrate to 0024: %v", err)
	}
	assertTableExists(t, sqlDB, "audit_anchors", false)
	assertAuditAnchorsPolicyCount(t, sqlDB, 0)
	assertAuditAnchorsForced(t, sqlDB, false)

	// Up to 0025: the table exists, RLS is ENABLE+FORCE, one policy.
	if err := goose.UpTo(sqlDB, "migrations", 25); err != nil {
		t.Fatalf("migrate to 0025: %v", err)
	}
	assertTableExists(t, sqlDB, "audit_anchors", true)
	assertAuditAnchorsPolicyCount(t, sqlDB, 1)
	assertAuditAnchorsForced(t, sqlDB, true)

	// Down to 0024: the table (and its RLS) must actually be gone.
	if err := goose.DownTo(sqlDB, "migrations", 24); err != nil {
		t.Fatalf("migrate down to 0024: %v", err)
	}
	assertTableExists(t, sqlDB, "audit_anchors", false)

	// Up again: re-applies cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 25); err != nil {
		t.Fatalf("migrate up to 0025 again: %v", err)
	}
	assertTableExists(t, sqlDB, "audit_anchors", true)
	assertAuditAnchorsPolicyCount(t, sqlDB, 1)
	assertAuditAnchorsForced(t, sqlDB, true)
}

// TestMigration0025_RowLevelSecurity proves audit_anchors' RLS actually
// protects the app request path, the same "superuser trap" / "FORCE trap"
// concern TestMigration0024_RowLevelSecurity guards against: a brand-new,
// non-superuser role made the table's OWNER (so FORCE, not just ENABLE, is
// what is actually under test), with app.tenant_id set or unset exercising
// both branches of the tenant_isolation policy.
func TestMigration0025_RowLevelSecurity(t *testing.T) {
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
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	const roleName = "rls_anchor_role"
	const rolePassword = "rls-anchor-role-pw" //nolint:gosec // test-only password for a throwaway container role
	mustExecDB(t, sqlDB, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, roleName, rolePassword))
	mustExecDB(t, sqlDB, fmt.Sprintf(`ALTER TABLE audit_anchors OWNER TO %s`, roleName))
	mustExecDB(t, sqlDB, fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO %s`, roleName))
	mustExecDB(t, sqlDB, fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO %s`, roleName))
	// tenants is referenced by no FK from audit_anchors, but the tenant rows
	// this test inserts (via the real repo, below) still need it.

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	appDSN := fmt.Sprintf("postgres://%s:%s@%s:%s/ledger?sslmode=disable", roleName, rolePassword, host, mappedPort.Port())

	appPool, err := postgres.NewPool(ctx, appDSN, 5)
	if err != nil {
		t.Fatalf("new pool as %s: %v", roleName, err)
	}
	defer appPool.Close()

	var isSuperuser bool
	if err := appPool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&isSuperuser); err != nil {
		t.Fatalf("check rolsuper: %v", err)
	}
	if isSuperuser {
		t.Fatalf("test role %s must not be a superuser, or this test proves nothing", roleName)
	}

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()

	// Two anchor rows, one per tenant, inserted directly (standing in for
	// the anchor job's own INSERT, which runs with the GUC unset): this test
	// is about audit_anchors' own RLS policy, not the job's insertion path,
	// which internal/audit's own tests already cover.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO audit_anchors (tenant_id, chain_seq, row_hash) VALUES ($1::uuid, 1, 'hash-a')`, tenantA,
	); err != nil {
		t.Fatalf("insert tenant A anchor: %v", err)
	}
	if err := rawExecAsTenant(ctx, appPool, tenantB,
		`INSERT INTO audit_anchors (tenant_id, chain_seq, row_hash) VALUES ($1::uuid, 1, 'hash-b')`, tenantB,
	); err != nil {
		t.Fatalf("insert tenant B anchor: %v", err)
	}

	// GUC set to tenant A: only tenant A's anchor is visible (the
	// forgotten-filter case, and the FORCE trap: without FORCE, the owning
	// role would see both regardless of the GUC).
	rowsA := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM audit_anchors`)
	assertAllTenant(t, "audit_anchors (tenant A)", rowsA, tenantA)
	if len(rowsA) == 0 {
		t.Error("audit_anchors (tenant A): expected tenant A's own anchor, got none")
	}

	// WITH CHECK: a write claiming tenant B's id while the GUC is set to A
	// is rejected.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO audit_anchors (tenant_id, chain_seq, row_hash) VALUES ($1::uuid, 2, 'cross-tenant')`, tenantB,
	); err == nil {
		t.Error("WITH CHECK: insert of a tenant B row while app.tenant_id=A was accepted, want a row-level security violation")
	}

	// GUC unset: the cross-tenant worker path (the anchor job itself) sees
	// every tenant's anchors.
	rowsUnset := rawQueryNoTenant(ctx, t, appPool, `SELECT DISTINCT tenant_id::text FROM audit_anchors`)
	if !containsTenant(rowsUnset, tenantA) || !containsTenant(rowsUnset, tenantB) {
		t.Errorf("audit_anchors with GUC unset: got tenants %v, want both %q and %q (the anchor job's cross-tenant access must keep working)",
			rowsUnset, tenantA, tenantB)
	}
}

// assertTableExists fails t unless the named table's presence in
// information_schema.tables matches want.
func assertTableExists(t *testing.T, db *sql.DB, table string, want bool) { //nolint:unparam // table is a general test-helper parameter; every current caller in this file happens to pass "audit_anchors"
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
		table,
	).Scan(&got); err != nil {
		t.Fatalf("check table %s exists: %v", table, err)
	}
	if got != want {
		t.Errorf("table %s exists = %v, want %v", table, got, want)
	}
}

// assertAuditAnchorsPolicyCount fails t unless exactly want tenant_isolation
// policies exist on audit_anchors.
func assertAuditAnchorsPolicyCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_policies WHERE tablename = 'audit_anchors' AND policyname = 'tenant_isolation'`,
	).Scan(&got); err != nil {
		t.Fatalf("count audit_anchors policies: %v", err)
	}
	if got != want {
		t.Errorf("audit_anchors tenant_isolation policy count = %d, want %d", got, want)
	}
}

// assertAuditAnchorsForced fails t unless audit_anchors' relforcerowsecurity
// matches want. It treats a missing table (post-down) as false, matching the
// caller's expectation of "not forced" in that state.
func assertAuditAnchorsForced(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	err := db.QueryRow(
		`SELECT relforcerowsecurity FROM pg_class WHERE relname = 'audit_anchors'`,
	).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		got = false
	case err != nil:
		t.Fatalf("check audit_anchors relforcerowsecurity: %v", err)
	}
	if got != want {
		t.Errorf("audit_anchors relforcerowsecurity = %v, want %v", got, want)
	}
}
