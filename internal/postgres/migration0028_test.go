package postgres_test

// Migration 0028 (ADR-018, Task 6.2 audit review fix) versions crypto_keys:
// a tenant can hold a sequence of DEK versions over time, keyed on
// (tenant_id, version), so a shred destroys only the CURRENT version and the
// tenant's next Encrypt call mints a fresh, forward one instead of failing
// closed forever. These tests prove the migration is cleanly reversible (the
// same up/down/up shape every other migration test in this package uses)
// and that the new schema actually behaves as versioned: two rows for the
// same tenant at different versions, a composite primary key, and RLS left
// completely undisturbed by any of it.

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

// TestMigration0028_Reversible proves migration 0028 is cleanly reversible:
// up adds the version column and switches the primary key to
// (tenant_id, version), down actually removes both (not merely "no error"),
// and up re-applies cleanly afterward. RLS (ENABLE + FORCE + the
// tenant_isolation policy from migration 0027) is untouched throughout.
func TestMigration0028_Reversible(t *testing.T) {
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

	// Up through 0027: crypto_keys exists, but has no version column yet and
	// is keyed on tenant_id alone.
	if err := goose.UpTo(sqlDB, "migrations", 27); err != nil {
		t.Fatalf("migrate to 0027: %v", err)
	}
	assertCryptoKeysHasVersionColumn(t, sqlDB, false)
	assertCryptoKeysPrimaryKeyColumns(t, sqlDB, []string{"tenant_id"})
	assertCryptoKeysPolicyCount(t, sqlDB, 1)
	assertCryptoKeysForced(t, sqlDB, true)

	// Up to 0028: version column exists, PK is (tenant_id, version), RLS is
	// completely unchanged.
	if err := goose.UpTo(sqlDB, "migrations", 28); err != nil {
		t.Fatalf("migrate to 0028: %v", err)
	}
	assertCryptoKeysHasVersionColumn(t, sqlDB, true)
	assertCryptoKeysPrimaryKeyColumns(t, sqlDB, []string{"tenant_id", "version"})
	assertCryptoKeysPolicyCount(t, sqlDB, 1)
	assertCryptoKeysForced(t, sqlDB, true)

	// A tenant can now hold two rows, one per version: the entire point of
	// this migration.
	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0028 test tenant')`, tenant)
	mustExecDB(t, sqlDB, `INSERT INTO crypto_keys (tenant_id, version, wrapped_dek) VALUES ($1, 1, $2)`, tenant, []byte("wrapped-v1"))
	mustExecDB(t, sqlDB, `INSERT INTO crypto_keys (tenant_id, version, wrapped_dek) VALUES ($1, 2, $2)`, tenant, []byte("wrapped-v2"))
	var rowCount int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM crypto_keys WHERE tenant_id = $1`, tenant).Scan(&rowCount); err != nil {
		t.Fatalf("count crypto_keys rows for tenant: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("crypto_keys rows for tenant with two DEK versions = %d, want 2", rowCount)
	}

	// Down to 0027: the version column and the composite PK are actually
	// gone (not merely "no error"), and crypto_keys is back to one row per
	// tenant (this migration's down path deliberately drops every version
	// but the first: see the migration's own Down comment).
	if err := goose.DownTo(sqlDB, "migrations", 27); err != nil {
		t.Fatalf("migrate down to 0027: %v", err)
	}
	assertCryptoKeysHasVersionColumn(t, sqlDB, false)
	assertCryptoKeysPrimaryKeyColumns(t, sqlDB, []string{"tenant_id"})
	assertCryptoKeysPolicyCount(t, sqlDB, 1)
	assertCryptoKeysForced(t, sqlDB, true)
	if err := sqlDB.QueryRow(`SELECT count(*) FROM crypto_keys WHERE tenant_id = $1`, tenant).Scan(&rowCount); err != nil {
		t.Fatalf("count crypto_keys rows for tenant after down: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("crypto_keys rows for tenant after down to 0027 = %d, want 1 (only version 1 survives)", rowCount)
	}

	// Up again: re-applies cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 28); err != nil {
		t.Fatalf("migrate up to 0028 again: %v", err)
	}
	assertCryptoKeysHasVersionColumn(t, sqlDB, true)
	assertCryptoKeysPrimaryKeyColumns(t, sqlDB, []string{"tenant_id", "version"})
	assertCryptoKeysPolicyCount(t, sqlDB, 1)
	assertCryptoKeysForced(t, sqlDB, true)
}

// assertCryptoKeysHasVersionColumn fails t unless crypto_keys.version exists
// (post-0028) or not (pre-0028), matching want.
func assertCryptoKeysHasVersionColumn(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var got bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'crypto_keys' AND column_name = 'version')`,
	).Scan(&got); err != nil {
		t.Fatalf("check crypto_keys.version column: %v", err)
	}
	if got != want {
		t.Errorf("crypto_keys has a version column = %v, want %v", got, want)
	}
}

// assertCryptoKeysPrimaryKeyColumns fails t unless crypto_keys' primary key
// is built from exactly want's columns, in order.
func assertCryptoKeysPrimaryKeyColumns(t *testing.T, db *sql.DB, want []string) {
	t.Helper()
	rows, err := db.Query(`
		SELECT a.attname
		FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = 'crypto_keys'::regclass AND i.indisprimary
		ORDER BY array_position(i.indkey, a.attnum)
	`)
	if err != nil {
		t.Fatalf("query crypto_keys primary key columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan primary key column: %v", err)
		}
		got = append(got, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate primary key columns: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("crypto_keys primary key columns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("crypto_keys primary key columns = %v, want %v", got, want)
			break
		}
	}
}
