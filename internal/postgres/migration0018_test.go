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

// TestMigration0018_ReferenceAndEffectiveAt runs migration 0018 in isolation
// (Task 4.3, audit A1.3): it migrates to 0017, inserts a transaction the way
// every pre-0018 row looks (no reference or effective_at columns yet),
// migrates forward through 0018, proves the unique partial index rejects a
// second reference in the same tenant while allowing the same reference in a
// different tenant and allowing many NULLs, reverses (dropping both columns
// and the index), and re-applies, proving the migration is cleanly reversible.
func TestMigration0018_ReferenceAndEffectiveAt(t *testing.T) {
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

	// Migrate to just before the new columns: 0017.
	if err := goose.UpTo(sqlDB, "migrations", 17); err != nil {
		t.Fatalf("migrate to 0017: %v", err)
	}

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0018 test tenant a')`, tenantA)
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0018 test tenant b')`, tenantB)

	// Migrate forward through 0018: both columns and the unique partial index
	// must all be in place.
	if err := goose.UpTo(sqlDB, "migrations", 18); err != nil {
		t.Fatalf("migrate to 0018: %v", err)
	}

	first := uuid.NewString()
	past := time.Now().Add(-2 * time.Second).UTC()
	mustExecDB(t, sqlDB,
		`INSERT INTO transactions (id, tenant_id, reference, effective_at) VALUES ($1, $2, $3, $4)`,
		first, tenantA, "INV-1001", past)

	var reference string
	var effectiveAt time.Time
	if err := sqlDB.QueryRow(
		`SELECT reference, effective_at FROM transactions WHERE id = $1`, first,
	).Scan(&reference, &effectiveAt); err != nil {
		t.Fatalf("read back reference and effective_at: %v", err)
	}
	if reference != "INV-1001" {
		t.Errorf("reference = %q, want INV-1001", reference)
	}
	if !effectiveAt.Equal(past) {
		t.Errorf("effective_at = %v, want %v", effectiveAt, past)
	}

	// The unique partial index must reject a SECOND transaction with the same
	// reference in the SAME tenant.
	if _, err := sqlDB.Exec(
		`INSERT INTO transactions (id, tenant_id, reference) VALUES ($1, $2, $3)`,
		uuid.NewString(), tenantA, "INV-1001",
	); err == nil {
		t.Error("expected a unique violation on a duplicate reference within one tenant, got nil")
	}

	// The SAME reference must be allowed for a DIFFERENT tenant.
	if _, err := sqlDB.Exec(
		`INSERT INTO transactions (id, tenant_id, reference) VALUES ($1, $2, $3)`,
		uuid.NewString(), tenantB, "INV-1001",
	); err != nil {
		t.Errorf("expected the same reference to be allowed in a different tenant, got error: %v", err)
	}

	// Many NULL references must be allowed in the same tenant: the partial
	// WHERE clause excludes NULL from the index entirely.
	for i := 0; i < 3; i++ {
		if _, err := sqlDB.Exec(
			`INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`,
			uuid.NewString(), tenantA,
		); err != nil {
			t.Errorf("expected a NULL reference to be allowed (attempt %d), got error: %v", i, err)
		}
	}

	// Down: both columns (and the index) must be reversed cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 17); err != nil {
		t.Fatalf("migrate down to 0017: %v", err)
	}
	for _, col := range []string{"reference", "effective_at"} {
		var columnExists bool
		if err := sqlDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'transactions' AND column_name = $1)`,
			col,
		).Scan(&columnExists); err != nil {
			t.Fatalf("check column transactions.%s exists: %v", col, err)
		}
		if columnExists {
			t.Errorf("transactions.%s still exists after migrating down to 0017", col)
		}
	}

	// Up again: must re-apply cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 18); err != nil {
		t.Fatalf("migrate up to 0018 again: %v", err)
	}
	for _, col := range []string{"reference", "effective_at"} {
		var columnExistsAgain bool
		if err := sqlDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'transactions' AND column_name = $1)`,
			col,
		).Scan(&columnExistsAgain); err != nil {
			t.Fatalf("check column exists after re-migrating: %v", err)
		}
		if !columnExistsAgain {
			t.Errorf("transactions.%s does not exist after migrating up to 0018 again", col)
		}
	}
}
