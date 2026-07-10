package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// startMigrateTestPostgres boots a disposable Postgres container for the
// migrate-subcommand tests, mirroring the wait strategy and skip-on-no-Docker
// behavior internal/postgres's own container tests use (see
// internal/postgres/migration0025_test.go): the readiness log appears twice
// (initdb, then the real server), so a port-only wait would race real
// readiness.
func startMigrateTestPostgres(t *testing.T) string {
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
	return dsn
}

// countEmbeddedMigrations counts the .sql files goose sees under the
// embedded migrations directory, so the "latest version" assertion below
// tracks the real migration set instead of a hardcoded number that would go
// stale the next time a migration is added.
func countEmbeddedMigrations(t *testing.T) int64 {
	t.Helper()
	entries, err := postgres.Migrations.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations dir: %v", err)
	}
	var n int64
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// TestRunMigrations_FreshDatabase_AppliesToLatestAndIsIdempotent proves the
// migrate subcommand's testable core (Task 5.6b, audit A4.3): against a
// fresh database, runMigrations applies every embedded migration up to the
// latest version (asserted both by the audit_anchors table, the newest
// migration at the time of writing, and by the goose schema version
// matching the number of embedded migration files), and running it again
// against an already-migrated database is a no-op, not an error. This is
// exactly what the deploy pipeline relies on when it runs
// `./go-ledger.new migrate` on every deploy, whether or not the database was
// already current.
func TestRunMigrations_FreshDatabase_AppliesToLatestAndIsIdempotent(t *testing.T) {
	dsn := startMigrateTestPostgres(t)
	ctx := context.Background()

	if err := runMigrations(ctx, dsn); err != nil {
		t.Fatalf("runMigrations (fresh database): %v", err)
	}
	assertTableExists(t, dsn, "audit_anchors", true)

	wantVersion := countEmbeddedMigrations(t)
	gotVersion, err := migrationStatus(ctx, dsn)
	if err != nil {
		t.Fatalf("migrationStatus after first run: %v", err)
	}
	if gotVersion != wantVersion {
		t.Errorf("schema version after first run = %d, want %d (one per embedded migration file)", gotVersion, wantVersion)
	}

	// Re-run: idempotent. No error, no change in version, table still there.
	if err := runMigrations(ctx, dsn); err != nil {
		t.Fatalf("runMigrations (re-run against already-migrated database): %v", err)
	}
	assertTableExists(t, dsn, "audit_anchors", true)
	gotVersion, err = migrationStatus(ctx, dsn)
	if err != nil {
		t.Fatalf("migrationStatus after second run: %v", err)
	}
	if gotVersion != wantVersion {
		t.Errorf("schema version after re-run = %d, want %d (re-running must not change it)", gotVersion, wantVersion)
	}
}

// TestRunMigrations_UnreachableDatabase_FailsClearly proves a `migrate`
// invocation against a database that cannot be reached fails with a
// non-nil, wrapped error rather than hanging or panicking, so the deploy
// pipeline's pre-swap step aborts loudly instead of timing out silently.
func TestRunMigrations_UnreachableDatabase_FailsClearly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Port 1 is never a live Postgres; this connects (or fails to) fast
	// rather than waiting out a long TCP timeout.
	err := runMigrations(ctx, "postgres://nobody:nobody@127.0.0.1:1/nope?sslmode=disable&connect_timeout=2")
	if err == nil {
		t.Fatal("runMigrations against an unreachable database: got nil error, want a connection error")
	}
}

// assertTableExists fails t unless the named table's presence in
// information_schema.tables (queried against dsn directly, not through the
// app's pgx pool) matches want.
func assertTableExists(t *testing.T, dsn, table string, want bool) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db to assert table existence: %v", err)
	}
	defer func() { _ = db.Close() }()

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

// TestRunMigrateCommand_RequiresDatabaseURL proves `migrate` (any
// subcommand) fails fast with a clear error when DATABASE_URL is unset,
// before ever attempting a database connection: an operator running it by
// hand with a missing env var gets an immediate, legible failure, not a
// hang or a generic driver error.
func TestRunMigrateCommand_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := runMigrateCommand(nil, logger)
	if err == nil {
		t.Fatal("runMigrateCommand with DATABASE_URL unset: got nil error, want an error")
	}
	if !errors.Is(err, errDatabaseURLRequired) {
		t.Errorf("runMigrateCommand with DATABASE_URL unset: got %v, want errDatabaseURLRequired", err)
	}
}

// TestRunMigrateCommand_UnknownSubcommand proves an unrecognized `migrate`
// subcommand fails clearly and, since the switch rejects it before any
// database work runs, does so without needing a reachable database: a typo
// like `migrate stauts` is rejected immediately rather than silently doing
// nothing or falling through to `up`.
func TestRunMigrateCommand_UnknownSubcommand(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example/db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := runMigrateCommand([]string{"bogus"}, logger)
	if err == nil {
		t.Fatal("runMigrateCommand with an unknown subcommand: got nil error, want an error")
	}
}
