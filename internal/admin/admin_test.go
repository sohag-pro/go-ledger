package admin_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sohag-pro/go-ledger/internal/admin"
	"github.com/sohag-pro/go-ledger/internal/auth"
	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// One Postgres container is shared across every test in this package,
// started once in TestMain, exactly like internal/postgres's own tests: each
// test scopes its data with a fresh tenant/key, so they never collide.
var (
	sharedPool *pgxpool.Pool
	poolErr    error
)

func TestMain(m *testing.M) {
	os.Exit(runWithContainer(m))
}

func runWithContainer(m *testing.M) int {
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ledger"),
		tcpostgres.WithUsername("ledger"),
		tcpostgres.WithPassword("ledger"),
		// Wait on the readiness log, not just the open port: Postgres opens
		// 5432 during initdb and then restarts it, so a port-only wait races
		// real readiness. The log line appears twice (initdb, then the real
		// server), hence WithOccurrence(2).
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		poolErr = fmt.Errorf("cannot start postgres container (is Docker running?): %w", err)
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		poolErr = err
		return m.Run()
	}
	if err := migrate(dsn); err != nil {
		poolErr = err
		return m.Run()
	}
	pool, err := postgres.NewPool(ctx, dsn, 10)
	if err != nil {
		poolErr = err
		return m.Run()
	}
	defer pool.Close()
	sharedPool = pool
	return m.Run()
}

func migrate(dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(postgres.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(sqlDB, "migrations")
}

// newTestSvc skips the test (rather than failing) when no container was
// available, so the suite stays green without Docker, and returns both the
// admin.Service under test and the underlying repository, which tests need
// directly for setup (e.g. suspending a tenant) that is not part of the
// admin surface itself.
func newTestSvc(t *testing.T) (*admin.Service, *postgres.Repository) {
	t.Helper()
	if poolErr != nil {
		t.Skipf("skipping integration test: %v", poolErr)
	}
	repo := postgres.NewRepository(sharedPool)
	return admin.NewService(repo), repo
}

// TestIssueKeyResolvesThroughRealAuthResolver proves the end-to-end path the
// brief calls out: a key minted by IssueKey resolves, through the real
// internal/auth.Resolver (not a fake), to the right tenant with the scopes
// and expiry that were requested.
func TestIssueKeyResolvesThroughRealAuthResolver(t *testing.T) {
	t.Parallel()
	svc, repo := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "issue-key resolve test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	plaintext, key, err := svc.IssueKey(ctx, tenant.ID, "ci key", []domain.Scope{domain.ScopeRead, domain.ScopePost}, &expiresAt)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if plaintext == "" {
		t.Fatal("expected non-empty plaintext")
	}
	if key.ID == "" {
		t.Fatal("expected an assigned key id")
	}

	resolver := auth.NewResolver(repo, time.Minute)
	resolved, err := resolver.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatalf("resolve issued key: %v", err)
	}
	if resolved.TenantID != tenant.ID {
		t.Errorf("resolved TenantID = %q, want %q", resolved.TenantID, tenant.ID)
	}
	if !resolved.HasScope(domain.ScopeRead) || !resolved.HasScope(domain.ScopePost) {
		t.Errorf("resolved Scopes = %v, want read and post", resolved.Scopes)
	}
	if resolved.HasScope(domain.ScopeAdmin) {
		t.Error("resolved key unexpectedly has admin scope")
	}
	if resolved.ExpiresAt == nil || !resolved.ExpiresAt.Equal(expiresAt) {
		t.Errorf("resolved ExpiresAt = %v, want %v", resolved.ExpiresAt, expiresAt)
	}
}

// TestRotateKeyOldStillResolvesUntilExplicitlyRevoked proves the overlap
// window: rotating a key mints a new working credential while the old one
// keeps resolving, exactly as documented on RotateKey.
func TestRotateKeyOldStillResolvesUntilExplicitlyRevoked(t *testing.T) {
	t.Parallel()
	svc, repo := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "rotate test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	oldPlaintext, oldKey, err := svc.IssueKey(ctx, tenant.ID, "rotate me", []domain.Scope{domain.ScopeRead}, nil)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}

	newPlaintext, newKey, err := svc.RotateKey(ctx, oldKey.ID)
	if err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if newPlaintext == oldPlaintext {
		t.Fatal("rotated plaintext must differ from the original")
	}
	if newKey.ID == oldKey.ID {
		t.Fatal("rotated key must have a new id")
	}
	if newKey.TenantID != oldKey.TenantID || newKey.Name != oldKey.Name {
		t.Errorf("rotated key tenant/name = %s/%s, want %s/%s", newKey.TenantID, newKey.Name, oldKey.TenantID, oldKey.Name)
	}
	if len(newKey.Scopes) != len(oldKey.Scopes) || newKey.Scopes[0] != oldKey.Scopes[0] {
		t.Errorf("rotated key scopes = %v, want %v", newKey.Scopes, oldKey.Scopes)
	}

	resolver := auth.NewResolver(repo, time.Minute)
	if _, err := resolver.Resolve(ctx, oldPlaintext); err != nil {
		t.Errorf("old key should still resolve after rotation: %v", err)
	}
	if _, err := resolver.Resolve(ctx, newPlaintext); err != nil {
		t.Errorf("new key should resolve: %v", err)
	}

	// Explicitly revoking the old key afterward is what actually cuts it off.
	if err := svc.RevokeKey(ctx, oldKey.ID); err != nil {
		t.Fatalf("revoke old key: %v", err)
	}
	// A fresh resolver, not the one above: Resolve caches a hit for its full
	// TTL regardless of subsequent revocation (see auth.Resolver's own doc
	// comment), so the resolver instance that already cached oldPlaintext
	// would still see it as good until that cache entry expires. A resolver
	// with no warm cache entry (a cold process, or this one after its TTL
	// lapses) hits the repository directly and sees the revocation
	// immediately, which is what this proves.
	freshResolver := auth.NewResolver(repo, time.Minute)
	if _, err := freshResolver.Resolve(ctx, oldPlaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("old key after explicit revoke: err = %v, want ErrUnauthorized", err)
	}
	if _, err := freshResolver.Resolve(ctx, newPlaintext); err != nil {
		t.Errorf("new key should still resolve after old key's revoke: %v", err)
	}
}

// TestRevokeKeyMakesOldKeyFailToResolve is the direct revoke-path proof
// (distinct from the rotate test above, which covers it via rotation first).
func TestRevokeKeyMakesOldKeyFailToResolve(t *testing.T) {
	t.Parallel()
	svc, repo := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "revoke test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	plaintext, key, err := svc.IssueKey(ctx, tenant.ID, "to revoke", []domain.Scope{domain.ScopePost}, nil)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}

	resolver := auth.NewResolver(repo, time.Minute)
	if _, err := resolver.Resolve(ctx, plaintext); err != nil {
		t.Fatalf("resolve before revoke: %v", err)
	}

	if err := svc.RevokeKey(ctx, key.ID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}

	// A fresh resolver: the one above already cached a successful resolve for
	// its full TTL, which Resolve does not invalidate on a later revocation
	// (see auth.Resolver's doc comment). One with no warm cache entry hits
	// the repository directly and sees the revocation immediately.
	freshResolver := auth.NewResolver(repo, time.Minute)
	if _, err := freshResolver.Resolve(ctx, plaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("resolve after revoke: err = %v, want ErrUnauthorized", err)
	}
}

// TestRevokeKeyUnknownIDErrors proves revoking a key id that never existed
// returns domain.ErrAPIKeyNotFound, and revoking the same real key twice is
// a no-op success rather than a second error (see RevokeAPIKey's doc).
func TestRevokeKeyUnknownIDErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	err := svc.RevokeKey(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Errorf("revoke unknown key: err = %v, want ErrAPIKeyNotFound", err)
	}

	tenant, err := svc.CreateTenant(ctx, "double revoke test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	_, key, err := svc.IssueKey(ctx, tenant.ID, "double revoke", []domain.Scope{domain.ScopeRead}, nil)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if err := svc.RevokeKey(ctx, key.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := svc.RevokeKey(ctx, key.ID); err != nil {
		t.Errorf("second revoke of the same key: err = %v, want nil (idempotent)", err)
	}
}

// TestIssueKeyIntoMissingTenantErrors proves issuing against a tenant id
// with no row fails closed with domain.ErrTenantNotFound, before any key is
// even generated.
func TestIssueKeyIntoMissingTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	_, _, err := svc.IssueKey(ctx, "00000000-0000-0000-0000-000000000000", "orphan", []domain.Scope{domain.ScopeRead}, nil)
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("issue key into missing tenant: err = %v, want ErrTenantNotFound", err)
	}
}

// TestIssueKeyIntoClosedTenantErrors and
// TestIssueKeyIntoSuspendedTenantErrors prove the tenant-status gate the
// brief requires: issuing into a non-active tenant fails closed with a
// *domain.TenantNotActiveError instead of silently minting a credential
// that cannot be used until the tenant is reactivated.
func TestIssueKeyIntoClosedTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "closed tenant test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := svc.SetTenantStatus(ctx, tenant.ID, domain.TenantClosed); err != nil {
		t.Fatalf("close tenant: %v", err)
	}

	_, _, err = svc.IssueKey(ctx, tenant.ID, "into closed", []domain.Scope{domain.ScopeRead}, nil)
	var tenantErr *domain.TenantNotActiveError
	if !errors.As(err, &tenantErr) {
		t.Fatalf("issue key into closed tenant: err = %v, want *domain.TenantNotActiveError", err)
	}
	if tenantErr.Status != domain.TenantClosed {
		t.Errorf("TenantNotActiveError.Status = %q, want %q", tenantErr.Status, domain.TenantClosed)
	}
}

func TestIssueKeyIntoSuspendedTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "suspended tenant test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := svc.SetTenantStatus(ctx, tenant.ID, domain.TenantSuspended); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	_, _, err = svc.IssueKey(ctx, tenant.ID, "into suspended", []domain.Scope{domain.ScopeRead}, nil)
	if !errors.Is(err, domain.ErrTenantNotActive) {
		t.Errorf("issue key into suspended tenant: err = %v, want ErrTenantNotActive", err)
	}
}

// TestRotateKeyIntoClosedTenantErrors proves the same tenant-active gate
// applies to RotateKey, even though the tenant id is derived from the old
// key rather than passed by the caller.
func TestRotateKeyIntoClosedTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "rotate into closed test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	_, key, err := svc.IssueKey(ctx, tenant.ID, "will rotate", []domain.Scope{domain.ScopeRead}, nil)
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	if err := svc.SetTenantStatus(ctx, tenant.ID, domain.TenantClosed); err != nil {
		t.Fatalf("close tenant: %v", err)
	}

	_, _, err = svc.RotateKey(ctx, key.ID)
	if !errors.Is(err, domain.ErrTenantNotActive) {
		t.Errorf("rotate key for closed tenant: err = %v, want ErrTenantNotActive", err)
	}
}

// TestRotateKeyUnknownIDErrors proves rotating a key id that never existed
// returns domain.ErrAPIKeyNotFound.
func TestRotateKeyUnknownIDErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	_, _, err := svc.RotateKey(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Errorf("rotate unknown key: err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestIssueKeyInvalidScopesErrors proves an empty or bogus scope list is
// rejected before ever reaching the repository.
func TestIssueKeyInvalidScopesErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "invalid scopes test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cases := []struct {
		name   string
		scopes []domain.Scope
	}{
		{"empty", nil},
		{"unknown scope", []domain.Scope{"write"}},
		{"valid mixed with unknown", []domain.Scope{domain.ScopeRead, "superuser"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := svc.IssueKey(ctx, tenant.ID, "bad scopes", tc.scopes, nil)
			if !errors.Is(err, admin.ErrInvalidScopes) {
				t.Errorf("IssueKey(scopes=%v): err = %v, want ErrInvalidScopes", tc.scopes, err)
			}
		})
	}
}

// TestListKeysNeverContainsPlaintext proves ListKeys surfaces every key's
// metadata (including a revoked one) but never a plaintext: domain.APIKey
// has no field capable of holding one (only ID/Name/Scopes/timestamps), so
// this test's real job is proving the metadata itself is correct, since the
// type system already rules out a plaintext leak.
func TestListKeysNeverContainsPlaintext(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "list keys test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, liveKey, err := svc.IssueKey(ctx, tenant.ID, "live", []domain.Scope{domain.ScopeRead}, nil)
	if err != nil {
		t.Fatalf("issue live key: %v", err)
	}
	_, revokedKey, err := svc.IssueKey(ctx, tenant.ID, "revoked", []domain.Scope{domain.ScopePost}, nil)
	if err != nil {
		t.Fatalf("issue revoked key: %v", err)
	}
	if err := svc.RevokeKey(ctx, revokedKey.ID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}

	keys, err := svc.ListKeys(ctx, tenant.ID)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListKeys returned %d keys, want 2", len(keys))
	}

	byID := make(map[string]domain.APIKey, len(keys))
	for _, k := range keys {
		byID[k.ID] = k
	}
	live, ok := byID[liveKey.ID]
	if !ok {
		t.Fatal("live key missing from ListKeys")
	}
	if live.RevokedAt != nil {
		t.Errorf("live key RevokedAt = %v, want nil", *live.RevokedAt)
	}
	if live.CreatedAt.IsZero() {
		t.Error("live key CreatedAt is zero, want a real timestamp")
	}
	revoked, ok := byID[revokedKey.ID]
	if !ok {
		t.Fatal("revoked key missing from ListKeys (list must include revoked history)")
	}
	if revoked.RevokedAt == nil {
		t.Error("revoked key RevokedAt = nil, want a real timestamp")
	}
}

// TestCreateTenantAndSetStatusRoundTrip is a small smoke test for the
// tenant-lifecycle passthrough methods, which are otherwise only exercised
// indirectly above via the tenant-gating tests.
func TestCreateTenantAndSetStatusRoundTrip(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "lifecycle test")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if tenant.Status != domain.TenantActive {
		t.Errorf("new tenant Status = %q, want active", tenant.Status)
	}

	tenants, err := svc.ListTenants(ctx)
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	found := false
	for _, tn := range tenants {
		if tn.ID == tenant.ID {
			found = true
		}
	}
	if !found {
		t.Error("ListTenants did not include the newly created tenant")
	}

	if err := svc.SetTenantStatus(ctx, tenant.ID, domain.TenantSuspended); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}
}

// TestSetFXRateInsertsAResolvableTenantRate is ledgerctl "rate set"'s
// underlying path (Task 2.4, audit A3.3): SetFXRate must not just insert a
// row without error, it must insert a row that fx.Provider.Rate actually
// resolves ahead of the global default for that tenant, and that a different
// tenant, with no row of its own, does not see.
func TestSetFXRateInsertsAResolvableTenantRate(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "fx rate test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	otherTenant, err := svc.CreateTenant(ctx, "fx rate test tenant (no rate of its own)")
	if err != nil {
		t.Fatalf("create other tenant: %v", err)
	}

	// A small past safety margin, not exactly time.Now(): CurrentFXRate gates
	// on "effective_at <= now()" using the database SERVER's clock, so a
	// timestamp from this test process landing even slightly ahead of the
	// server's clock would make the row transiently invisible immediately
	// after insert (the same clock-skew class of bug Task 2.4 fixed for the
	// omitted-effective-at case; here the test passes an explicit timestamp,
	// so the margin is applied here instead).
	effectiveAt := time.Now().UTC().Add(-2 * time.Second)
	if err := svc.SetFXRate(ctx, tenant.ID, "USD", "TRY", 3_000_000_00, 120, "manual", &effectiveAt); err != nil {
		t.Fatalf("SetFXRate: %v", err)
	}

	provider := fx.NewDBProvider(sharedPool)
	quote, spreadBps, err := provider.Rate(ctx, tenant.ID, "USD", "TRY")
	if err != nil {
		t.Fatalf("Rate(tenant) error = %v", err)
	}
	if quote.MidRateE8 != 3_000_000_00 || spreadBps != 120 {
		t.Errorf("Rate(tenant) = {mid: %d, spread: %d}, want {mid: 300000000, spread: 120} (the row SetFXRate inserted)",
			quote.MidRateE8, spreadBps)
	}
	if quote.Source != "manual" {
		t.Errorf("Rate(tenant).Source = %q, want manual", quote.Source)
	}

	// A different tenant, with no USD/TRY row of its own, must NOT resolve
	// the row SetFXRate just inserted for tenant: it has no global default
	// for this pair either, so it must fail with ErrFXRateNotFound.
	_, _, err = provider.Rate(ctx, otherTenant.ID, "USD", "TRY")
	if !errors.Is(err, domain.ErrFXRateNotFound) {
		t.Errorf("Rate(otherTenant) = %v, want ErrFXRateNotFound (tenant-scoped rate must not leak to another tenant)", err)
	}
}

// TestSetFXRateMissingTenantErrors proves SetFXRate fails closed with
// domain.ErrTenantNotFound for a tenant id that does not exist, rather than
// surfacing a raw foreign-key-violation error from the database.
func TestSetFXRateMissingTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	err := svc.SetFXRate(ctx, "00000000-0000-0000-0000-000000000000", "USD", "TRY", 100_000_000, 0, "manual", nil)
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("SetFXRate into missing tenant: err = %v, want ErrTenantNotFound", err)
	}
}

// TestSetFXRateValidatesBeforeInsert proves SetFXRate rejects a malformed
// rate or spread the same way internal/fx.Seed rejects a malformed FX_RATES
// entry: before ever touching the database, using the domain errors the
// fx_rates CHECK constraints mirror.
func TestSetFXRateValidatesBeforeInsert(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "fx rate validation test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cases := []struct {
		name      string
		base      domain.Currency
		quote     domain.Currency
		midRateE8 int64
		spreadBps int32
		wantErr   error
	}{
		{"same currency", "USD", "USD", 100_000_000, 0, domain.ErrSameCurrencyRate},
		{"non-positive rate", "USD", "TRY", 0, 0, domain.ErrNonPositiveRate},
		{"spread too wide", "USD", "TRY", 100_000_000, 10_000, domain.ErrInvalidSpread},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := svc.SetFXRate(ctx, tenant.ID, tc.base, tc.quote, tc.midRateE8, tc.spreadBps, "manual", nil)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("SetFXRate(%s/%s, mid=%d, spread=%d): err = %v, want %v",
					tc.base, tc.quote, tc.midRateE8, tc.spreadBps, err, tc.wantErr)
			}
		})
	}
}

// TestSetTenantPolicyRoundTrip proves ledgerctl "tenant policy"'s underlying
// path (Task 2.4b, audit A3.4): SetTenantPolicy must not just write without
// error, GetTenant must read back exactly the policy that was set, decoded
// through the same domain.ParseTenantSettings the ledger's enforcement path
// uses.
func TestSetTenantPolicyRoundTrip(t *testing.T) {
	t.Parallel()
	svc, repo := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "policy test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	policy := domain.TenantPolicy{
		MaxTransactionAmount: 500_00,
		DailyVolumeLimit:     2_000_00,
		AllowedCurrencies:    []string{"USD", "EUR"},
	}
	if err := svc.SetTenantPolicy(ctx, tenant.ID, policy); err != nil {
		t.Fatalf("SetTenantPolicy: %v", err)
	}

	got, err := repo.GetTenant(ctx, tenant.ID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	settings, err := domain.ParseTenantSettings(got.Settings)
	if err != nil {
		t.Fatalf("ParseTenantSettings: %v", err)
	}
	if settings.Policy.MaxTransactionAmount != policy.MaxTransactionAmount {
		t.Errorf("MaxTransactionAmount = %d, want %d", settings.Policy.MaxTransactionAmount, policy.MaxTransactionAmount)
	}
	if settings.Policy.DailyVolumeLimit != policy.DailyVolumeLimit {
		t.Errorf("DailyVolumeLimit = %d, want %d", settings.Policy.DailyVolumeLimit, policy.DailyVolumeLimit)
	}
	if len(settings.Policy.AllowedCurrencies) != 2 {
		t.Fatalf("AllowedCurrencies = %v, want [USD EUR]", settings.Policy.AllowedCurrencies)
	}

	// Setting a second, DIFFERENT policy overwrites the first (whole-document
	// replace, not a merge, per SetTenantPolicy's doc comment).
	if err := svc.SetTenantPolicy(ctx, tenant.ID, domain.TenantPolicy{MaxTransactionAmount: 1}); err != nil {
		t.Fatalf("SetTenantPolicy (overwrite): %v", err)
	}
	got, err = repo.GetTenant(ctx, tenant.ID)
	if err != nil {
		t.Fatalf("GetTenant (after overwrite): %v", err)
	}
	settings, err = domain.ParseTenantSettings(got.Settings)
	if err != nil {
		t.Fatalf("ParseTenantSettings (after overwrite): %v", err)
	}
	if settings.Policy.MaxTransactionAmount != 1 || settings.Policy.DailyVolumeLimit != 0 || len(settings.Policy.AllowedCurrencies) != 0 {
		t.Errorf("policy after overwrite = %+v, want only MaxTransactionAmount=1", settings.Policy)
	}
}

// TestSetTenantPolicyMissingTenantErrors proves SetTenantPolicy fails closed
// with domain.ErrTenantNotFound for a tenant id that does not exist, rather
// than silently no-op'ing (SetTenantSettings's execrows is 0, which the
// repository maps to ErrTenantNotFound, the same pattern SetTenantStatus and
// SetFXRate already use).
func TestSetTenantPolicyMissingTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	err := svc.SetTenantPolicy(ctx, "00000000-0000-0000-0000-000000000000", domain.TenantPolicy{MaxTransactionAmount: 100})
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("SetTenantPolicy on missing tenant: err = %v, want ErrTenantNotFound", err)
	}
}

// TestSetTenantPolicyValidatesBeforeWrite proves SetTenantPolicy rejects a
// malformed policy (a negative limit, or a currency that is not a
// well-formed three-letter code) before ever writing anything, the same
// defense-in-depth style TestSetFXRateValidatesBeforeInsert covers for
// SetFXRate above.
func TestSetTenantPolicyValidatesBeforeWrite(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "policy validation test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cases := []struct {
		name   string
		policy domain.TenantPolicy
	}{
		{"negative max transaction amount", domain.TenantPolicy{MaxTransactionAmount: -1}},
		{"negative daily volume limit", domain.TenantPolicy{DailyVolumeLimit: -1}},
		{"malformed currency code", domain.TenantPolicy{AllowedCurrencies: []string{"usd"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := svc.SetTenantPolicy(ctx, tenant.ID, tc.policy)
			if !errors.Is(err, domain.ErrInvalidTenantPolicy) {
				t.Errorf("SetTenantPolicy(%+v): err = %v, want ErrInvalidTenantPolicy", tc.policy, err)
			}
		})
	}
}

// TestSetTenantPolicyThenPostRespectsIt is the full admin-to-ledger path
// (Task 2.4b, audit A3.4): a policy set through admin.Service.SetTenantPolicy
// is enforced the very next time that tenant posts through
// ledger.TransactionService.Post, over the same repository, with no
// intervening step. This is the "an operator sets a policy, then a post
// respects it" case the task brief calls out explicitly.
func TestSetTenantPolicyThenPostRespectsIt(t *testing.T) {
	t.Parallel()
	svc, repo := newTestSvc(t)
	txSvc := ledger.NewTransactionService(repo, nil, nil)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "policy enforcement test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := svc.SetTenantPolicy(ctx, tenant.ID, domain.TenantPolicy{MaxTransactionAmount: 100}); err != nil {
		t.Fatalf("SetTenantPolicy: %v", err)
	}

	debit := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	credit := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant.ID, debit); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant.ID, credit); err != nil {
		t.Fatalf("create credit account: %v", err)
	}

	d, _ := domain.NewMoney(101, "USD")
	c, _ := domain.NewMoney(-101, "USD")
	over := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: debit.ID, Amount: d},
		{AccountID: credit.ID, Amount: c},
	}}
	_, err = txSvc.Post(ctx, tenant.ID, over, nil)
	var pv *domain.PolicyViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("post over the just-set policy cap: err = %v, want *domain.PolicyViolationError", err)
	}
	if pv.Rule != domain.PolicyRuleMaxTransactionAmount {
		t.Errorf("PolicyViolationError.Rule = %s, want %s", pv.Rule, domain.PolicyRuleMaxTransactionAmount)
	}
}

// TestCreateWebhookSubscriptionReturnsSecretOnceAndNeverInList proves the
// Task 4.1 (audit A7.1) once-only secret discipline: CreateWebhookSubscription
// returns a non-empty secret, and ListWebhookSubscriptions afterward carries
// the subscription's metadata but has no field capable of returning that
// secret again (domain.WebhookSubscription itself has no secret field).
func TestCreateWebhookSubscriptionReturnsSecretOnceAndNeverInList(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "webhook create test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	secret, sub, err := svc.CreateWebhookSubscription(ctx, tenant.ID, "https://example.com/hooks", []string{domain.ActionTransactionCreated})
	if err != nil {
		t.Fatalf("create webhook subscription: %v", err)
	}
	if secret == "" {
		t.Fatal("expected a non-empty secret")
	}
	if sub.ID == "" {
		t.Fatal("expected an assigned subscription id")
	}
	if !sub.Active {
		t.Error("expected a newly created subscription to be active")
	}

	subs, err := svc.ListWebhookSubscriptions(ctx, tenant.ID)
	if err != nil {
		t.Fatalf("list webhook subscriptions: %v", err)
	}
	if len(subs) != 1 || subs[0].ID != sub.ID {
		t.Fatalf("ListWebhookSubscriptions = %v, want exactly [%s]", subs, sub.ID)
	}
	// domain.WebhookSubscription has no field capable of holding a secret at
	// all, so there is nothing more to assert here beyond "it compiles and
	// the metadata round-trips": that absence of a field is itself the
	// guarantee a list call can never leak one.
}

// TestCreateWebhookSubscriptionInvalidURLErrors proves the URL is validated
// before a secret is generated or any row is written: an empty or
// non-http(s) URL fails with domain.ErrInvalidWebhookURL.
func TestCreateWebhookSubscriptionInvalidURLErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "webhook invalid url test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"no scheme", "example.com/hooks"},
		{"wrong scheme", "ftp://example.com/hooks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := svc.CreateWebhookSubscription(ctx, tenant.ID, tc.url, nil)
			if !errors.Is(err, domain.ErrInvalidWebhookURL) {
				t.Errorf("CreateWebhookSubscription(url=%q): err = %v, want ErrInvalidWebhookURL", tc.url, err)
			}
		})
	}
}

// TestCreateWebhookSubscriptionMissingTenantErrors proves creating a
// subscription for a tenant id with no row fails closed with
// domain.ErrTenantNotFound, before any secret is generated.
func TestCreateWebhookSubscriptionMissingTenantErrors(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	_, _, err := svc.CreateWebhookSubscription(ctx, "00000000-0000-0000-0000-000000000000", "https://example.com/hooks", nil)
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("CreateWebhookSubscription into missing tenant: err = %v, want ErrTenantNotFound", err)
	}
}

// TestDeleteWebhookSubscriptionDeactivatesAndErrorsOnUnknownID proves
// DeleteWebhookSubscription deactivates rather than removing the row (still
// listed, just Active=false) and returns domain.ErrWebhookSubscriptionNotFound
// for an id that never existed.
func TestDeleteWebhookSubscriptionDeactivatesAndErrorsOnUnknownID(t *testing.T) {
	t.Parallel()
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	tenant, err := svc.CreateTenant(ctx, "webhook delete test tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	_, sub, err := svc.CreateWebhookSubscription(ctx, tenant.ID, "https://example.com/hooks", nil)
	if err != nil {
		t.Fatalf("create webhook subscription: %v", err)
	}

	if err := svc.DeleteWebhookSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("delete webhook subscription: %v", err)
	}
	subs, err := svc.ListWebhookSubscriptions(ctx, tenant.ID)
	if err != nil {
		t.Fatalf("list webhook subscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subscriptions after delete = %d, want still 1 (deactivated, not removed)", len(subs))
	}
	if subs[0].Active {
		t.Error("subscription Active = true after delete, want false")
	}

	err = svc.DeleteWebhookSubscription(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, domain.ErrWebhookSubscriptionNotFound) {
		t.Errorf("delete unknown webhook subscription: err = %v, want ErrWebhookSubscriptionNotFound", err)
	}
}
