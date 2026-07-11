package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// startAdminProvisionTestPostgres boots a disposable, migrated Postgres
// container for the boot-equivalent provisioning tests below, mirroring the
// wait strategy and skip-on-no-Docker behavior startMigrateTestPostgres
// already uses in this package (see migrate_test.go): the readiness log
// appears twice (initdb, then the real server), so a port-only wait would
// race real readiness.
func startAdminProvisionTestPostgres(t *testing.T) string {
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
	if err := runMigrations(ctx, dsn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return dsn
}

// TestProvisionAdminKey_DemoBoot_DemoKeyResolvesWithAdminScope proves the
// demo half of ADR-019's "First-boot admin provisioning" end to end, against
// a real database: booting with DEMO_MODE on runs provisionAPIKeys (which
// wires demoKeyScopes into the demo key) and then provisionAdminKey (a
// no-op in demo mode). The resulting demo key resolves, through the real
// internal/auth.Resolver, with admin scope, so the public operator console
// can exercise admin panels against the demo tenant.
func TestProvisionAdminKey_DemoBoot_DemoKeyResolvesWithAdminScope(t *testing.T) {
	dsn := startAdminProvisionTestPostgres(t)
	ctx := context.Background()

	pool, err := postgres.NewPool(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()
	repo := postgres.NewRepository(pool)
	adminSvc := admin.NewService(repo)
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	cfg := config{
		defaultTenant:  "00000000-0000-0000-0000-000000000001",
		demoMode:       true,
		demoAPIKey:     "glk_integration_test_demo_key",
		adminBootstrap: true,
	}

	if err := provisionAPIKeys(ctx, repo, cfg, logger); err != nil {
		t.Fatalf("provisionAPIKeys: %v", err)
	}
	if err := provisionAdminKey(ctx, repo, adminSvc, cfg, logger); err != nil {
		t.Fatalf("provisionAdminKey (demo boot): %v", err)
	}

	resolver := auth.NewResolver(repo, time.Minute)
	key, err := resolver.Resolve(ctx, "Bearer "+cfg.demoAPIKey)
	if err != nil {
		t.Fatalf("resolve demo key: %v", err)
	}
	if !key.HasScope(domain.ScopeAdmin) {
		t.Errorf("demo key scopes = %v, want admin scope included in demo mode", key.Scopes)
	}
	if key.TenantID != cfg.defaultTenant {
		t.Errorf("demo key tenant = %q, want %q", key.TenantID, cfg.defaultTenant)
	}

	// provisionAdminKey is a no-op in demo mode: it must not have minted a
	// second, separate bootstrap-admin key alongside the demo one.
	keys, err := adminSvc.ListKeys(ctx, cfg.defaultTenant)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("tenant has %d keys after a demo boot, want 1 (only the demo key, no separate bootstrap-admin key)", len(keys))
	}
}

// TestProvisionAdminKey_ProdBoot_ProvisionsOnceAndIsIdempotent proves the
// production half of ADR-019 end to end, against a real database: a
// boot-equivalent call with no admin key yet present mints one and logs its
// plaintext exactly once, and a second boot-equivalent call (simulating a
// restart) finds the admin key already there, mints nothing further, and
// logs nothing about it, leaving the key count unchanged.
func TestProvisionAdminKey_ProdBoot_ProvisionsOnceAndIsIdempotent(t *testing.T) {
	dsn := startAdminProvisionTestPostgres(t)
	ctx := context.Background()

	pool, err := postgres.NewPool(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()
	repo := postgres.NewRepository(pool)
	adminSvc := admin.NewService(repo)

	cfg := config{
		defaultTenant:  "00000000-0000-0000-0000-000000000001",
		demoMode:       false,
		adminBootstrap: true,
	}

	// First boot: no admin key exists yet. provisionAPIKeys is a no-op here
	// (demoMode is false and no LOAD_TEST_API_KEY is set), mirroring what
	// run() does before calling provisionAdminKey.
	if err := provisionAPIKeys(ctx, repo, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatalf("provisionAPIKeys (first boot): %v", err)
	}
	var firstBootLog bytes.Buffer
	if err := provisionAdminKey(ctx, repo, adminSvc, cfg, slog.New(slog.NewTextHandler(&firstBootLog, nil))); err != nil {
		t.Fatalf("provisionAdminKey (first boot): %v", err)
	}
	if !strings.Contains(firstBootLog.String(), "provisioned bootstrap admin key") {
		t.Errorf("first boot log missing the one-time bootstrap-admin notice: %q", firstBootLog.String())
	}

	keysAfterFirstBoot, err := adminSvc.ListKeys(ctx, cfg.defaultTenant)
	if err != nil {
		t.Fatalf("list keys after first boot: %v", err)
	}
	if len(keysAfterFirstBoot) != 1 {
		t.Fatalf("keys after first boot = %d, want 1", len(keysAfterFirstBoot))
	}
	if !keysAfterFirstBoot[0].HasScope(domain.ScopeAdmin) {
		t.Errorf("provisioned key scopes = %v, want admin", keysAfterFirstBoot[0].Scopes)
	}
	mintedKeyID := keysAfterFirstBoot[0].ID

	// Second boot (a restart): the admin key from the first boot is already
	// there. provisionAdminKey must mint nothing further and log nothing.
	var secondBootLog bytes.Buffer
	if err := provisionAdminKey(ctx, repo, adminSvc, cfg, slog.New(slog.NewTextHandler(&secondBootLog, nil))); err != nil {
		t.Fatalf("provisionAdminKey (second boot): %v", err)
	}
	if secondBootLog.Len() != 0 {
		t.Errorf("second boot log = %q, want nothing (admin key already exists, idempotent)", secondBootLog.String())
	}

	keysAfterSecondBoot, err := adminSvc.ListKeys(ctx, cfg.defaultTenant)
	if err != nil {
		t.Fatalf("list keys after second boot: %v", err)
	}
	if len(keysAfterSecondBoot) != 1 {
		t.Errorf("keys after second boot = %d, want still 1 (idempotent, no duplicate mint)", len(keysAfterSecondBoot))
	}
	if keysAfterSecondBoot[0].ID != mintedKeyID {
		t.Errorf("key id changed across the second boot: got %q, want the same key %q minted on the first boot", keysAfterSecondBoot[0].ID, mintedKeyID)
	}
}
