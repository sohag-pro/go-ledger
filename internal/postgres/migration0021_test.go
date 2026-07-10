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

// TestMigration0021_Webhooks runs migration 0021 in isolation (Task 4.1,
// audit A7.1), the same up/down/up pattern the other single-migration tests
// in this package use: it migrates to 0020, migrates forward through 0021,
// proves the two new tables and the singleton cursor row exist, proves the
// webhook_deliveries UNIQUE (subscription_id, audit_chain_seq) constraint
// rejects a duplicate fan-out row, reverses (down to 0020, dropping all
// three tables), and re-applies (up to 0021 again), proving the migration is
// cleanly reversible.
func TestMigration0021_Webhooks(t *testing.T) {
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

	if err := goose.UpTo(sqlDB, "migrations", 20); err != nil {
		t.Fatalf("migrate to 0020: %v", err)
	}

	if err := goose.UpTo(sqlDB, "migrations", 21); err != nil {
		t.Fatalf("migrate to 0021: %v", err)
	}

	for _, table := range []string{"webhook_subscriptions", "webhook_deliveries", "webhook_fanout_cursor"} {
		var exists bool
		if err := sqlDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("check table %s exists: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist after migrating up to 0021", table)
		}
	}

	var cursor int64
	if err := sqlDB.QueryRow(`SELECT last_chain_seq FROM webhook_fanout_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read singleton cursor row: %v", err)
	}
	if cursor != 0 {
		t.Errorf("initial webhook_fanout_cursor.last_chain_seq = %d, want 0", cursor)
	}

	// Set up enough fixture data to insert a real webhook_deliveries row: a
	// tenant, a transaction, a subscription, and one chained audit_log row
	// (chain_seq is a plain column here, not the chainer's own sequence
	// default logic under test, so this test writes it directly).
	tenant := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO tenants (id, name) VALUES ($1, 'migration 0021 test tenant')`, tenant)
	txnID := uuid.NewString()
	mustExecDB(t, sqlDB, `INSERT INTO transactions (id, tenant_id) VALUES ($1, $2)`, txnID, tenant)
	subID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO webhook_subscriptions (id, tenant_id, url, secret) VALUES ($1, $2, 'https://example.com/hooks', 'whsec_x')`,
		subID, tenant)

	deliveryID := uuid.NewString()
	mustExecDB(t, sqlDB,
		`INSERT INTO webhook_deliveries (id, tenant_id, subscription_id, audit_chain_seq, event_type, payload)
		 VALUES ($1, $2, $3, 1, 'transaction.created', '{}')`,
		deliveryID, tenant, subID)

	// A second attempt at the SAME (subscription_id, audit_chain_seq) pairing
	// must be rejected by the unique index: this is what makes a re-run
	// fan-out pass exactly-once into this table.
	dupID := uuid.NewString()
	_, err = sqlDB.Exec(
		`INSERT INTO webhook_deliveries (id, tenant_id, subscription_id, audit_chain_seq, event_type, payload)
		 VALUES ($1, $2, $3, 1, 'transaction.created', '{}')`,
		dupID, tenant, subID)
	if err == nil {
		t.Error("expected a unique-violation inserting a duplicate (subscription_id, audit_chain_seq) pairing, got nil")
	}

	// A bad status value must be rejected by the CHECK constraint.
	badStatusID := uuid.NewString()
	_, err = sqlDB.Exec(
		`INSERT INTO webhook_deliveries (id, tenant_id, subscription_id, audit_chain_seq, event_type, payload, status)
		 VALUES ($1, $2, $3, 2, 'transaction.created', '{}', 'bogus')`,
		badStatusID, tenant, subID)
	if err == nil {
		t.Error("expected a CHECK violation inserting status='bogus', got nil")
	}

	// Down: all three tables must be reversed cleanly.
	if err := goose.DownTo(sqlDB, "migrations", 20); err != nil {
		t.Fatalf("migrate down to 0020: %v", err)
	}
	for _, table := range []string{"webhook_subscriptions", "webhook_deliveries", "webhook_fanout_cursor"} {
		var exists bool
		if err := sqlDB.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table,
		).Scan(&exists); err != nil {
			t.Fatalf("check table %s exists after down: %v", table, err)
		}
		if exists {
			t.Errorf("table %s still exists after migrating down to 0020", table)
		}
	}

	// Up again: must re-apply cleanly.
	if err := goose.UpTo(sqlDB, "migrations", 21); err != nil {
		t.Fatalf("migrate up to 0021 again: %v", err)
	}
	var cursorAgain int64
	if err := sqlDB.QueryRow(`SELECT last_chain_seq FROM webhook_fanout_cursor`).Scan(&cursorAgain); err != nil {
		t.Fatalf("read singleton cursor row after re-apply: %v", err)
	}
	if cursorAgain != 0 {
		t.Errorf("re-applied webhook_fanout_cursor.last_chain_seq = %d, want 0", cursorAgain)
	}
}
