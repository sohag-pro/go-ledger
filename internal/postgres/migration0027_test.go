package postgres_test

// Migration 0027 (Task 6.2, audit A9.3) adds crypto_keys: the per-tenant Data
// Encryption Key table behind internal/crypto.Cipher's envelope encryption of
// posting descriptions. It follows the exact same RLS shape migration 0024
// established (ENABLE + FORCE + one allow-when-unset tenant_isolation
// policy), so these tests mirror migration0025_test.go's own reversibility
// and RLS-behavior tests, scoped to this one new table. Unlike
// audit_anchors, crypto_keys has a real foreign key to tenants (its rows are
// meaningless without a tenant to belong to), so the RLS test seeds two real
// tenant rows before inserting crypto_keys rows for them.

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

// TestMigration0027_Reversible proves migration 0027 is cleanly reversible:
// up creates crypto_keys with exactly one tenant_isolation policy and FORCE
// set, down actually removes all of that (not merely "no error"), and up
// re-applies cleanly afterward. Same up/down/up shape as
// TestMigration0025_Reversible.
func TestMigration0027_Reversible(t *testing.T) {
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

	// Up through 0026: no crypto_keys table yet.
	if err := goose.UpTo(sqlDB, "migrations", 26); err != nil {
		t.Fatalf("migrate to 0026: %v", err)
	}
	assertTableExists(t, sqlDB, "crypto_keys", false)
	assertCryptoKeysPolicyCount(t, sqlDB, 0)
	assertCryptoKeysForced(t, sqlDB, false)

	// Up to 0027: the table exists, RLS is ENABLE+FORCE, one policy.
	if err := goose.UpTo(sqlDB, "migrations", 27); err != nil {
		t.Fatalf("migrate to 0027: %v", err)
	}
	assertTableExists(t, sqlDB, "crypto_keys", true)
	assertCryptoKeysPolicyCount(t, sqlDB, 1)
	assertCryptoKeysForced(t, sqlDB, true)

	// Down to 0026: the table (and its RLS) must actually be gone.
	if err := goose.DownTo(sqlDB, "migrations", 26); err != nil {
		t.Fatalf("migrate down to 0026: %v", err)
	}
	assertTableExists(t, sqlDB, "crypto_keys", false)

	// Up again: re-applies cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 27); err != nil {
		t.Fatalf("migrate up to 0027 again: %v", err)
	}
	assertTableExists(t, sqlDB, "crypto_keys", true)
	assertCryptoKeysPolicyCount(t, sqlDB, 1)
	assertCryptoKeysForced(t, sqlDB, true)
}

// TestMigration0027_RowLevelSecurity proves crypto_keys' RLS actually
// protects the app request path, the same "superuser trap" / "FORCE trap"
// concern TestMigration0024_RowLevelSecurity and TestMigration0025_RowLevelSecurity
// guard against: a brand-new, non-superuser role made the table's OWNER (so
// FORCE, not just ENABLE, is what is actually under test), with
// app.tenant_id set or unset exercising both branches of the tenant_isolation
// policy.
func TestMigration0027_RowLevelSecurity(t *testing.T) {
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
	// goose.Up runs every migration, including 0028 (ADR-018's versioned
	// crypto_keys): the raw INSERTs below name the version column
	// explicitly (version 1) to match that later schema, even though this
	// test is otherwise scoped to 0027's own RLS shape.
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'tenant a')`, tenantA)
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'tenant b')`, tenantB)

	const roleName = "rls_crypto_key_role"
	const rolePassword = "rls-crypto-key-role-pw" //nolint:gosec // test-only password for a throwaway container role
	mustExecDB(t, sqlDB, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, roleName, rolePassword))
	mustExecDB(t, sqlDB, fmt.Sprintf(`ALTER TABLE crypto_keys OWNER TO %s`, roleName))
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

	// Two key rows, one per tenant, inserted directly (standing in for
	// internal/crypto.Cipher's own GetOrCreateWrappedDEK, which runs with the
	// GUC set via the same withTenant path this test exercises directly):
	// this test is about crypto_keys' own RLS policy, not the cipher's
	// wrapping logic, which internal/crypto's own tests already cover.
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO crypto_keys (tenant_id, version, wrapped_dek) VALUES ($1::uuid, 1, $2)`, tenantA, []byte("wrapped-a"),
	); err != nil {
		t.Fatalf("insert tenant A crypto key: %v", err)
	}
	if err := rawExecAsTenant(ctx, appPool, tenantB,
		`INSERT INTO crypto_keys (tenant_id, version, wrapped_dek) VALUES ($1::uuid, 1, $2)`, tenantB, []byte("wrapped-b"),
	); err != nil {
		t.Fatalf("insert tenant B crypto key: %v", err)
	}

	// GUC set to tenant A: only tenant A's key is visible (the
	// forgotten-filter case, and the FORCE trap: without FORCE, the owning
	// role would see both regardless of the GUC).
	rowsA := rawQueryAsTenant(ctx, t, appPool, tenantA, `SELECT tenant_id::text FROM crypto_keys`)
	assertAllTenant(t, "crypto_keys (tenant A)", rowsA, tenantA)
	if len(rowsA) == 0 {
		t.Error("crypto_keys (tenant A): expected tenant A's own key, got none")
	}

	// WITH CHECK: a write claiming tenant B's id while the GUC is set to A is
	// rejected.
	tenantC := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'tenant c')`, tenantC)
	if err := rawExecAsTenant(ctx, appPool, tenantA,
		`INSERT INTO crypto_keys (tenant_id, version, wrapped_dek) VALUES ($1::uuid, 1, $2)`, tenantC, []byte("cross-tenant"),
	); err == nil {
		t.Error("WITH CHECK: insert of a tenant C row while app.tenant_id=A was accepted, want a row-level security violation")
	}

	// GUC unset: a cross-tenant caller (none exists for this table today, but
	// the backstop is uniform) sees every tenant's keys.
	rowsUnset := rawQueryNoTenant(ctx, t, appPool, `SELECT DISTINCT tenant_id::text FROM crypto_keys`)
	if !containsTenant(rowsUnset, tenantA) || !containsTenant(rowsUnset, tenantB) {
		t.Errorf("crypto_keys with GUC unset: got tenants %v, want both %q and %q",
			rowsUnset, tenantA, tenantB)
	}
}

// assertCryptoKeysPolicyCount fails t unless exactly want tenant_isolation
// policies exist on crypto_keys.
func assertCryptoKeysPolicyCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_policies WHERE tablename = 'crypto_keys' AND policyname = 'tenant_isolation'`,
	).Scan(&got); err != nil {
		t.Fatalf("count crypto_keys policies: %v", err)
	}
	if got != want {
		t.Errorf("crypto_keys tenant_isolation policy count = %d, want %d", got, want)
	}
}

// assertCryptoKeysForced fails t unless crypto_keys' relforcerowsecurity
// matches want. It treats a missing table (post-down) as false, matching the
// caller's expectation of "not forced" in that state.
func assertCryptoKeysForced(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	err := db.QueryRow(
		`SELECT relforcerowsecurity FROM pg_class WHERE relname = 'crypto_keys'`,
	).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		got = false
	case err != nil:
		t.Fatalf("check crypto_keys relforcerowsecurity: %v", err)
	}
	if got != want {
		t.Errorf("crypto_keys relforcerowsecurity = %v, want %v", got, want)
	}
}
