package postgres_test

// Migration 0032 (ADR-023) adds account hierarchy: a nullable
// accounts.parent_id self-reference, a composite accounts_parent_fk foreign
// key (tenant_id, parent_id) reusing accounts' own UNIQUE (tenant_id, id) so
// a cross-tenant parent cannot be inserted, an accounts_parent_idx index for
// the parent lookups the guard trigger and rollup queries do, and the
// accounts_hierarchy_guard trigger/function pair that rejects a self-parent,
// a cycle, or a child/parent currency mismatch. This test mirrors
// migration0031_test.go's reversibility shape and asserts the down migration
// actually removes all five of those objects, not merely "no error."
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

// newMigration0032TestDB starts a fresh Postgres container and returns a
// *sql.DB wired for goose, migrated up through 0031 (the state immediately
// before this migration). It skips the test rather than failing it when no
// container can be started, matching every other migration test in this
// package.
func newMigration0032TestDB(t *testing.T) *sql.DB {
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
	if err := goose.UpTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate to 0031: %v", err)
	}
	return sqlDB
}

// TestMigration0032_Reversible proves migration 0032 is cleanly reversible:
// up adds accounts.parent_id, the accounts_parent_fk constraint, the
// accounts_parent_idx index, and the accounts_hierarchy_guard
// function/trigger pair; down removes every one of those five objects (not
// merely "no error"); and up re-applies cleanly afterward.
func TestMigration0032_Reversible(t *testing.T) {
	sqlDB := newMigration0032TestDB(t)

	assertAccountsParentIDColumnExists(t, sqlDB, false)
	assertAccountsParentFKExists(t, sqlDB, false)
	assertAccountsParentIdxExists(t, sqlDB, false)
	assertAccountsHierarchyGuardFunctionExists(t, sqlDB, false)
	assertAccountsHierarchyGuardTriggerExists(t, sqlDB, false)

	if err := goose.UpTo(sqlDB, "migrations", 32); err != nil {
		t.Fatalf("migrate to 0032: %v", err)
	}
	assertAccountsParentIDColumnExists(t, sqlDB, true)
	assertAccountsParentFKExists(t, sqlDB, true)
	assertAccountsParentIdxExists(t, sqlDB, true)
	assertAccountsHierarchyGuardFunctionExists(t, sqlDB, true)
	assertAccountsHierarchyGuardTriggerExists(t, sqlDB, true)

	if err := goose.DownTo(sqlDB, "migrations", 31); err != nil {
		t.Fatalf("migrate down to 0031: %v", err)
	}
	assertAccountsParentIDColumnExists(t, sqlDB, false)
	assertAccountsParentFKExists(t, sqlDB, false)
	assertAccountsParentIdxExists(t, sqlDB, false)
	assertAccountsHierarchyGuardFunctionExists(t, sqlDB, false)
	assertAccountsHierarchyGuardTriggerExists(t, sqlDB, false)

	if err := goose.UpTo(sqlDB, "migrations", 32); err != nil {
		t.Fatalf("migrate up to 0032 again: %v", err)
	}
	assertAccountsParentIDColumnExists(t, sqlDB, true)
	assertAccountsParentFKExists(t, sqlDB, true)
	assertAccountsParentIdxExists(t, sqlDB, true)
	assertAccountsHierarchyGuardFunctionExists(t, sqlDB, true)
	assertAccountsHierarchyGuardTriggerExists(t, sqlDB, true)
}

// assertAccountsParentIDColumnExists fails t unless accounts.parent_id's
// existence matches want.
func assertAccountsParentIDColumnExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'accounts' AND column_name = 'parent_id'
		)`,
	).Scan(&got); err != nil {
		t.Fatalf("check accounts.parent_id existence: %v", err)
	}
	if got != want {
		t.Errorf("accounts.parent_id column exists = %v, want %v", got, want)
	}
}

// assertAccountsParentFKExists fails t unless the accounts_parent_fk foreign
// key's existence matches want.
func assertAccountsParentFKExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.table_constraints
			WHERE table_name = 'accounts' AND constraint_name = 'accounts_parent_fk'
		)`,
	).Scan(&got); err != nil {
		t.Fatalf("check accounts_parent_fk existence: %v", err)
	}
	if got != want {
		t.Errorf("accounts_parent_fk constraint exists = %v, want %v", got, want)
	}
}

// assertAccountsParentIdxExists fails t unless the accounts_parent_idx
// index's existence matches want.
func assertAccountsParentIdxExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE tablename = 'accounts' AND indexname = 'accounts_parent_idx'
		)`,
	).Scan(&got); err != nil {
		t.Fatalf("check accounts_parent_idx existence: %v", err)
	}
	if got != want {
		t.Errorf("accounts_parent_idx index exists = %v, want %v", got, want)
	}
}

// assertAccountsHierarchyGuardFunctionExists fails t unless the
// accounts_hierarchy_guard function's existence matches want.
func assertAccountsHierarchyGuardFunctionExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'accounts_hierarchy_guard')`,
	).Scan(&got); err != nil {
		t.Fatalf("check accounts_hierarchy_guard function existence: %v", err)
	}
	if got != want {
		t.Errorf("accounts_hierarchy_guard function exists = %v, want %v", got, want)
	}
}

// assertAccountsHierarchyGuardTriggerExists fails t unless the
// accounts_hierarchy_guard_trg trigger's existence matches want.
func assertAccountsHierarchyGuardTriggerExists(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.triggers
			WHERE event_object_table = 'accounts' AND trigger_name = 'accounts_hierarchy_guard_trg'
		)`,
	).Scan(&got); err != nil {
		t.Fatalf("check accounts_hierarchy_guard_trg trigger existence: %v", err)
	}
	if got != want {
		t.Errorf("accounts_hierarchy_guard_trg trigger exists = %v, want %v", got, want)
	}
}
