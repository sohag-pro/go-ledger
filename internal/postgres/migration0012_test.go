package postgres_test

import (
	"context"
	"database/sql"
	"strings"
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

// TestMigration0012_ScopesDefaultAndCheckConstraint runs migration 0012 in
// isolation against its own Postgres container (Task 2.2), the same pattern
// TestMigration0011 uses: it migrates to 0011, inserts a pre-2.2 api_keys row
// the way real existing data would look, migrates forward to 0012, and checks
// the row picked up the {read,post} default. It then proves the new CHECK
// constraint (api_keys_scopes_valid) rejects both an empty scopes array and
// an unknown scope value, reverses (down to 0011, dropping the constraint and
// the three columns), and re-applies (up to 0012 again), proving the
// migration is cleanly reversible (up, down, up).
func TestMigration0012_ScopesDefaultAndCheckConstraint(t *testing.T) {
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

	// Migrate to just before the lifecycle columns: 0011.
	if err := goose.UpTo(sqlDB, "migrations", 11); err != nil {
		t.Fatalf("migrate to 0011: %v", err)
	}

	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0012 test tenant')`, tenant)

	preExistingKeyID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash) VALUES ($1, $2, 'pre-2.2 key', $3)`,
		preExistingKeyID, tenant, uuid.NewString())

	// Migrate forward through 0012: the pre-existing row must pick up the
	// {read,post} default, and the demo/load-test keys keep working exactly
	// as before.
	if err := goose.UpTo(sqlDB, "migrations", 12); err != nil {
		t.Fatalf("migrate to 0012: %v", err)
	}
	assertScopes(t, sqlDB, preExistingKeyID, []string{"read", "post"})
	assertNullExpiryAndLastUsed(t, sqlDB, preExistingKeyID)

	// The CHECK constraint rejects an empty scopes array.
	if _, err := sqlDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, 'empty scopes', $3, ARRAY[]::text[])`,
		uuid.NewString(), tenant, uuid.NewString(),
	); err == nil {
		t.Error("expected an empty scopes array to be rejected by api_keys_scopes_valid, got nil")
	}

	// The CHECK constraint rejects an unknown scope value.
	if _, err := sqlDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, 'bad scope', $3, ARRAY['superuser'])`,
		uuid.NewString(), tenant, uuid.NewString(),
	); err == nil {
		t.Error("expected an unknown scope value to be rejected by api_keys_scopes_valid, got nil")
	}

	// A valid, non-default scope set is accepted.
	adminKeyID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes) VALUES ($1, $2, 'admin key', $3, ARRAY['read','post','admin'])`,
		adminKeyID, tenant, uuid.NewString())
	assertScopes(t, sqlDB, adminKeyID, []string{"read", "post", "admin"})

	// Down: the constraint and the three columns must all go away cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 11); err != nil {
		t.Fatalf("migrate down to 0011: %v", err)
	}
	for _, col := range []string{"scopes", "expires_at", "last_used_at"} {
		var columnExists bool
		if err := sqlDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'api_keys' AND column_name = $1)`,
			col,
		).Scan(&columnExists); err != nil {
			t.Fatalf("check column %s exists: %v", col, err)
		}
		if columnExists {
			t.Errorf("api_keys.%s still exists after migrating down to 0011", col)
		}
	}

	// Up again: must re-apply cleanly, and the default must still work.
	if err := goose.UpTo(sqlDB, "migrations", 12); err != nil {
		t.Fatalf("migrate up to 0012 again: %v", err)
	}
	secondKeyID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash) VALUES ($1, $2, 'post-redo key', $3)`,
		secondKeyID, tenant, uuid.NewString())
	assertScopes(t, sqlDB, secondKeyID, []string{"read", "post"})
}

// assertScopes checks api_keys.scopes for keyID against want. It scans the
// array cast to text (e.g. "{read,post}") rather than into a Go []string:
// this test drives goose through a bare database/sql *sql.DB (matching
// TestMigration0011's pattern), which has no array-scanning support wired up
// the way the pgx pool connections elsewhere in this package do.
func assertScopes(t *testing.T, db *sql.DB, keyID string, want []string) {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT scopes::text FROM api_keys WHERE id = $1`, keyID).Scan(&raw); err != nil {
		t.Fatalf("get scopes for key %s: %v", keyID, err)
	}
	got := strings.Split(strings.Trim(raw, "{}"), ",")
	if len(got) != len(want) {
		t.Fatalf("scopes for key %s = %v (raw %q), want %v", keyID, got, raw, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scopes for key %s = %v (raw %q), want %v", keyID, got, raw, want)
			break
		}
	}
}

func assertNullExpiryAndLastUsed(t *testing.T, db *sql.DB, keyID string) {
	t.Helper()
	var expiresAt, lastUsedAt sql.NullTime
	if err := db.QueryRow(`SELECT expires_at, last_used_at FROM api_keys WHERE id = $1`, keyID).Scan(&expiresAt, &lastUsedAt); err != nil {
		t.Fatalf("get expires_at/last_used_at for key %s: %v", keyID, err)
	}
	if expiresAt.Valid {
		t.Errorf("expires_at for pre-existing key %s = %v, want NULL", keyID, expiresAt.Time)
	}
	if lastUsedAt.Valid {
		t.Errorf("last_used_at for pre-existing key %s = %v, want NULL", keyID, lastUsedAt.Time)
	}
}
