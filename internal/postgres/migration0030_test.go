package postgres_test

// Migration 0030 (follow-up F1, audit N1 from the final review) closes the
// one gap in migration 0024's own defense-in-depth story: api_keys is
// tenant-scoped (tenant_id, migrations 0008/0011) but was left out of
// migration 0024's row-level security sweep. It follows the exact same RLS
// shape (ENABLE + FORCE + one allow-when-unset tenant_isolation policy), so
// these tests mirror migration0027_test.go / migration0029_test.go's own
// reversibility and RLS-behavior tests, scoped to this one table. Every
// current caller of api_keys (the auth resolver's GetAPIKeyByHash, the admin
// surface's ListAPIKeysByTenant/InsertAPIKey/RevokeAPIKey, and cmd/server's
// boot-time provisionAPIKeys) runs with the GUC unset, so this migration
// changes nothing about their behavior; the RLS test below proves both the
// GUC-set backstop and the GUC-unset "everything still works" path.

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

// TestMigration0030_Reversible proves migration 0030 is cleanly reversible:
// up adds exactly one tenant_isolation policy and FORCE to api_keys, down
// actually removes both (not merely "no error"), and up re-applies cleanly
// afterward.
func TestMigration0030_Reversible(t *testing.T) {
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

	// Up through 0029: api_keys exists (since migration 0008) but has no RLS.
	if err := goose.UpTo(sqlDB, "migrations", 29); err != nil {
		t.Fatalf("migrate to 0029: %v", err)
	}
	assertAPIKeysPolicyCount(t, sqlDB, 0)
	assertAPIKeysForced(t, sqlDB, false)

	// Up to 0030: api_keys gets ENABLE+FORCE and exactly one policy.
	if err := goose.UpTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate to 0030: %v", err)
	}
	assertAPIKeysPolicyCount(t, sqlDB, 1)
	assertAPIKeysForced(t, sqlDB, true)

	// Down to 0029: RLS must actually be gone, and NO FORCE must have run
	// before DISABLE (migration 0030's own down comment).
	if err := goose.DownTo(sqlDB, "migrations", 29); err != nil {
		t.Fatalf("migrate down to 0029: %v", err)
	}
	assertAPIKeysPolicyCount(t, sqlDB, 0)
	assertAPIKeysForced(t, sqlDB, false)

	// Up again: re-applies cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 30); err != nil {
		t.Fatalf("migrate up to 0030 again: %v", err)
	}
	assertAPIKeysPolicyCount(t, sqlDB, 1)
	assertAPIKeysForced(t, sqlDB, true)
}

// TestMigration0030_RowLevelSecurity proves api_keys' RLS actually protects
// the app request path (the "superuser trap" / "FORCE trap" every other RLS
// migration test in this package guards against, see
// TestMigration0024_RowLevelSecurity's doc comment for the full reasoning),
// and that every existing GUC-unset caller (the auth resolver, the admin
// surface) keeps seeing every tenant's keys unchanged.
func TestMigration0030_RowLevelSecurity(t *testing.T) {
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
	if err := repo.CreateTenant(ctx, tenantA, "api keys rls tenant a"); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if err := repo.CreateTenant(ctx, tenantB, "api keys rls tenant b"); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}

	const roleName = "rls_api_key_role"
	const rolePassword = "rls-api-key-role-pw" //nolint:gosec // test-only password for a throwaway container role
	mustExecDB(t, sqlDB, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, roleName, rolePassword))
	mustExecDB(t, sqlDB, fmt.Sprintf(`ALTER TABLE api_keys OWNER TO %s`, roleName))
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

	// One key row per tenant, inserted directly (standing in for
	// postgres.Repository.InsertAPIKey, which today always runs on the pool
	// with the GUC unset, exercised separately below): this test is about
	// api_keys' own RLS policy, not the repository's hashing/assembly logic.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash) VALUES ($1::uuid, $2::uuid, 'key a', $3)`,
		uuid.NewString(), tenantA, "hash-a",
	); err != nil {
		t.Fatalf("insert tenant A api key: %v", err)
	}
	if err := rawExecAsTenant(ctx, appPool, tenantB,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash) VALUES ($1::uuid, $2::uuid, 'key b', $3)`,
		uuid.NewString(), tenantB, "hash-b",
	); err != nil {
		t.Fatalf("insert tenant B api key: %v", err)
	}

	// GUC set to tenant A: only tenant A's key is visible (the
	// forgotten-filter case, and the FORCE trap: without FORCE, the owning
	// role would see both regardless of the GUC).
	rowsA := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM api_keys`)
	assertAllTenant(t, "api_keys (tenant A)", rowsA, tenantA)
	if len(rowsA) == 0 {
		t.Error("api_keys (tenant A): expected tenant A's own key, got none")
	}

	// WITH CHECK: a write claiming tenant B's id while the GUC is set to A is
	// rejected.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash) VALUES ($1::uuid, $2::uuid, 'cross-tenant', $3)`,
		uuid.NewString(), tenantB, "hash-cross",
	); err == nil {
		t.Error("WITH CHECK: insert of a tenant B row while app.tenant_id=A was accepted, want a row-level security violation")
	}

	// GUC unset: every current caller of api_keys (auth resolver, admin
	// surface, boot-time provisioning) runs this way and must keep seeing
	// every tenant's keys.
	rowsUnset := rawQueryNoTenant(ctx, t, appPool, `SELECT DISTINCT tenant_id::text FROM api_keys`)
	if !containsTenant(rowsUnset, tenantA) || !containsTenant(rowsUnset, tenantB) {
		t.Errorf("api_keys with GUC unset: got tenants %v, want both %q and %q", rowsUnset, tenantA, tenantB)
	}

	// Sanity: the repository's own InsertAPIKey/GetAPIKeyByHash/
	// ListAPIKeysByTenant (the real auth resolver and admin paths, all
	// GUC-unset) still work end to end through this RLS-restricted,
	// non-superuser role.
	appRepo := postgres.NewRepository(appPool)
	newKey := domain.APIKey{TenantID: tenantA, Name: "via repository"}
	if err := appRepo.InsertAPIKey(ctx, newKey, "hash-via-repo"); err != nil {
		t.Fatalf("InsertAPIKey via repository (RLS-restricted role): %v", err)
	}
	resolved, err := appRepo.GetAPIKeyByHash(ctx, "hash-via-repo")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash via repository (RLS-restricted role): %v", err)
	}
	if resolved.TenantID != tenantA {
		t.Errorf("GetAPIKeyByHash: got tenant %q, want %q", resolved.TenantID, tenantA)
	}
	listed, err := appRepo.ListAPIKeysByTenant(ctx, tenantB)
	if err != nil {
		t.Fatalf("ListAPIKeysByTenant via repository (RLS-restricted role): %v", err)
	}
	if len(listed) == 0 {
		t.Error("ListAPIKeysByTenant tenant B: expected at least the seeded key, got none")
	}
}

// assertAPIKeysPolicyCount fails t unless exactly want tenant_isolation
// policies exist on api_keys.
func assertAPIKeysPolicyCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_policies WHERE tablename = 'api_keys' AND policyname = 'tenant_isolation'`,
	).Scan(&got); err != nil {
		t.Fatalf("count api_keys policies: %v", err)
	}
	if got != want {
		t.Errorf("api_keys tenant_isolation policy count = %d, want %d", got, want)
	}
}

// assertAPIKeysForced fails t unless api_keys' relforcerowsecurity matches
// want.
func assertAPIKeysForced(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT relforcerowsecurity FROM pg_class WHERE relname = 'api_keys'`,
	).Scan(&got); err != nil {
		t.Fatalf("check api_keys relforcerowsecurity: %v", err)
	}
	if got != want {
		t.Errorf("api_keys relforcerowsecurity = %v, want %v", got, want)
	}
}
