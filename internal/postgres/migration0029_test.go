package postgres_test

// Migration 0029 (Task 6.3, audit A9.2) adds disputes: a dispute/chargeback
// data model built on the reversal primitive (Task 4.2). It follows the
// exact same RLS shape migration 0024/0027 established (ENABLE + FORCE + one
// allow-when-unset tenant_isolation policy), so these tests mirror
// migration0027_test.go's own reversibility and RLS-behavior tests, scoped
// to this one new table. disputes also carries a composite tenant FK on
// (tenant_id, transaction_id) REFERENCES transactions (tenant_id, id) (Task
// 5.4a pattern, migration 0023), so the RLS test seeds a real tenant and
// transaction before inserting dispute rows.

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

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestMigration0029_Reversible proves migration 0029 is cleanly reversible:
// up creates disputes with exactly one tenant_isolation policy and FORCE
// set, down actually removes all of that (not merely "no error"), and up
// re-applies cleanly afterward.
func TestMigration0029_Reversible(t *testing.T) {
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

	// Up through 0028: no disputes table yet.
	if err := goose.UpTo(sqlDB, "migrations", 28); err != nil {
		t.Fatalf("migrate to 0028: %v", err)
	}
	assertTableExists(t, sqlDB, "disputes", false)
	assertDisputesPolicyCount(t, sqlDB, 0)
	assertDisputesForced(t, sqlDB, false)

	// Up to 0029: the table exists, RLS is ENABLE+FORCE, one policy.
	if err := goose.UpTo(sqlDB, "migrations", 29); err != nil {
		t.Fatalf("migrate to 0029: %v", err)
	}
	assertTableExists(t, sqlDB, "disputes", true)
	assertDisputesPolicyCount(t, sqlDB, 1)
	assertDisputesForced(t, sqlDB, true)

	// Down to 0028: the table (and its RLS) must actually be gone, and NO
	// FORCE must have run before DISABLE (migration 0029's own down comment).
	if err := goose.DownTo(sqlDB, "migrations", 28); err != nil {
		t.Fatalf("migrate down to 0028: %v", err)
	}
	assertTableExists(t, sqlDB, "disputes", false)

	// Up again: re-applies cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 29); err != nil {
		t.Fatalf("migrate up to 0029 again: %v", err)
	}
	assertTableExists(t, sqlDB, "disputes", true)
	assertDisputesPolicyCount(t, sqlDB, 1)
	assertDisputesForced(t, sqlDB, true)
}

// TestMigration0029_RowLevelSecurity proves disputes' RLS actually protects
// the app request path (the "superuser trap" / "FORCE trap" concern every
// other RLS migration test in this package guards against), and that the
// composite tenant FK rejects a dispute naming another tenant's transaction.
func TestMigration0029_RowLevelSecurity(t *testing.T) {
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

	pool, err := postgres.NewPool(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()
	repo := postgres.NewRepository(pool)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenantA, "disputes rls tenant a"); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if err := repo.CreateTenant(ctx, tenantB, "disputes rls tenant b"); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}
	txnA, _, _ := seedTxn(t, repo, tenantA)
	txnB, _, _ := seedTxn(t, repo, tenantB)

	const roleName = "rls_dispute_role"
	const rolePassword = "rls-dispute-role-pw" //nolint:gosec // test-only password for a throwaway container role
	mustExecDB(t, sqlDB, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, roleName, rolePassword))
	mustExecDB(t, sqlDB, fmt.Sprintf(`ALTER TABLE disputes OWNER TO %s`, roleName))
	mustExecDB(t, sqlDB, fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO %s`, roleName))
	mustExecDB(t, sqlDB, fmt.Sprintf(`GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO %s`, roleName))

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

	// One dispute row per tenant, inserted directly (standing in for
	// postgres.Repository.CreateDispute, which runs with the GUC set via the
	// same withTenant path this test exercises directly): this test is about
	// disputes' own RLS policy and FK, not the repository's assembly logic,
	// which internal/ledger's own dispute service tests cover.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO disputes (id, tenant_id, transaction_id, reason) VALUES ($1::uuid, $2::uuid, $3::uuid, 'a')`,
		uuid.NewString(), tenantA, txnA,
	); err != nil {
		t.Fatalf("insert tenant A dispute: %v", err)
	}
	if err := rawExecAsTenant(ctx, appPool, tenantB,
		`INSERT INTO disputes (id, tenant_id, transaction_id, reason) VALUES ($1::uuid, $2::uuid, $3::uuid, 'b')`,
		uuid.NewString(), tenantB, txnB,
	); err != nil {
		t.Fatalf("insert tenant B dispute: %v", err)
	}

	// GUC set to tenant A: only tenant A's dispute is visible (the
	// forgotten-filter case, and the FORCE trap: without FORCE, the owning
	// role would see both regardless of the GUC).
	rowsA := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM disputes`)
	assertAllTenant(t, "disputes (tenant A)", rowsA, tenantA)
	if len(rowsA) == 0 {
		t.Error("disputes (tenant A): expected tenant A's own dispute, got none")
	}

	// WITH CHECK: a write claiming tenant B's id while the GUC is set to A is
	// rejected.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO disputes (id, tenant_id, transaction_id, reason) VALUES ($1::uuid, $2::uuid, $3::uuid, 'cross-tenant')`,
		uuid.NewString(), tenantB, txnB,
	); err == nil {
		t.Error("WITH CHECK: insert of a tenant B row while app.tenant_id=A was accepted, want a row-level security violation")
	}

	// GUC unset: a cross-tenant caller sees every tenant's disputes.
	rowsUnset := rawQueryNoTenant(ctx, t, appPool, `SELECT DISTINCT tenant_id::text FROM disputes`)
	if !containsTenant(rowsUnset, tenantA) || !containsTenant(rowsUnset, tenantB) {
		t.Errorf("disputes with GUC unset: got tenants %v, want both %q and %q", rowsUnset, tenantA, tenantB)
	}

	// The composite tenant FK (disputes_txn_fk): a dispute claiming tenant A
	// but naming tenant B's transaction is rejected at the database, even
	// with the GUC unset (a plain FK violation, nothing to do with RLS).
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO disputes (id, tenant_id, transaction_id, reason) VALUES ($1::uuid, $2::uuid, $3::uuid, 'wrong tenant txn')`,
		uuid.NewString(), tenantA, txnB,
	); err == nil {
		t.Error("composite FK: insert naming tenant A but tenant B's transaction was accepted, want a foreign key violation")
	}

	// Sanity: the repository's own CreateDispute (through withTenant, the
	// real request path) also succeeds through this RLS-restricted,
	// non-superuser role for a correctly matched (tenant, transaction) pair.
	appRepo := postgres.NewRepository(appPool)
	d := &domain.Dispute{TransactionID: txnA, Reason: "via repository"}
	if err := appRepo.CreateDispute(ctx, tenantA, d); err != nil {
		t.Errorf("CreateDispute via repository (RLS-restricted role): %v", err)
	}
}

// assertDisputesPolicyCount fails t unless exactly want tenant_isolation
// policies exist on disputes.
func assertDisputesPolicyCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_policies WHERE tablename = 'disputes' AND policyname = 'tenant_isolation'`,
	).Scan(&got); err != nil {
		t.Fatalf("count disputes policies: %v", err)
	}
	if got != want {
		t.Errorf("disputes tenant_isolation policy count = %d, want %d", got, want)
	}
}

// assertDisputesForced fails t unless disputes' relforcerowsecurity matches
// want. It treats a missing table (post-down) as false.
func assertDisputesForced(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	err := db.QueryRow(
		`SELECT relforcerowsecurity FROM pg_class WHERE relname = 'disputes'`,
	).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		got = false
	case err != nil:
		t.Fatalf("check disputes relforcerowsecurity: %v", err)
	}
	if got != want {
		t.Errorf("disputes relforcerowsecurity = %v, want %v", got, want)
	}
}
