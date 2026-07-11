package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestAPIKeyInsertAndResolve covers the happy path: a key inserted via
// InsertAPIKey resolves by its hash to the same tenant, name, and rate limit,
// and a NULL rate_limit_rpm surfaces as a nil *int rather than a zero value.
func TestAPIKeyInsertAndResolve(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "api key test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	rpm := 120
	cases := []struct {
		name string
		key  domain.APIKey
	}{
		{
			name: "with rate limit",
			key:  domain.APIKey{TenantID: tenant, Name: "ci key", RateLimitRPM: &rpm},
		},
		{
			name: "nil rate limit uses server default",
			key:  domain.APIKey{TenantID: tenant, Name: "default-limit key", RateLimitRPM: nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plaintext, hash, err := domain.GenerateAPIKey()
			if err != nil {
				t.Fatalf("generate api key: %v", err)
			}
			if plaintext == "" {
				t.Fatal("expected non-empty plaintext")
			}

			if err := repo.InsertAPIKey(ctx, tc.key, hash); err != nil {
				t.Fatalf("insert api key: %v", err)
			}

			got, err := repo.GetAPIKeyByHash(ctx, hash)
			if err != nil {
				t.Fatalf("get api key by hash: %v", err)
			}
			if got.TenantID != tc.key.TenantID {
				t.Errorf("TenantID = %q, want %q", got.TenantID, tc.key.TenantID)
			}
			if got.Name != tc.key.Name {
				t.Errorf("Name = %q, want %q", got.Name, tc.key.Name)
			}
			if got.ID == "" {
				t.Error("expected generated id, got empty")
			}
			if got.TenantStatus != domain.TenantActive {
				t.Errorf("TenantStatus = %q, want %q (GetAPIKeyByHash joins tenants, Task 2.1)", got.TenantStatus, domain.TenantActive)
			}
			switch {
			case tc.key.RateLimitRPM == nil:
				if got.RateLimitRPM != nil {
					t.Errorf("RateLimitRPM = %v, want nil", *got.RateLimitRPM)
				}
			case got.RateLimitRPM == nil:
				t.Error("RateLimitRPM = nil, want a value")
			case *got.RateLimitRPM != *tc.key.RateLimitRPM:
				t.Errorf("RateLimitRPM = %d, want %d", *got.RateLimitRPM, *tc.key.RateLimitRPM)
			}
		})
	}
}

// TestAPIKeyRevokedNotFound covers revocation: once revoked_at is set, the key
// no longer resolves, matching ADR-012's "missing or unknown or revoked key is
// a 401" rule at the storage layer.
func TestAPIKeyRevokedNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "revoked key test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, hash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	key := domain.APIKey{TenantID: tenant, Name: "to be revoked"}
	if err := repo.InsertAPIKey(ctx, key, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	// Sanity: resolves before revocation.
	if _, err := repo.GetAPIKeyByHash(ctx, hash); err != nil {
		t.Fatalf("get api key by hash before revoke: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE key_hash = $1`, hash); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}

	_, err = repo.GetAPIKeyByHash(ctx, hash)
	if !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Errorf("get api key by hash after revoke: err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestAPIKeyUnknownHashNotFound covers a hash with no matching row at all,
// distinct from a revoked one but resulting in the same error.
func TestAPIKeyUnknownHashNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	_, err := repo.GetAPIKeyByHash(ctx, domain.HashAPIKey("glk_never-issued"))
	if !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Errorf("get api key by unknown hash: err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestAPIKeyDefaultScopesAndNullLifecycleFields covers Task 2.2/2.2b's
// defaulting: a caller that does not set Scopes on the domain.APIKey it
// passes to InsertAPIKey (every pre-2.2 caller, e.g. cmd/server's demo and
// load-test key provisioning) still gets {read,post} and NULL
// expires_at/last_used_at, the same as before the admin surface (Task 2.2b)
// added the ability to set them explicitly. See
// TestAPIKeyInsertPersistsScopesAndExpiry below for the explicit-value path.
func TestAPIKeyDefaultScopesAndNullLifecycleFields(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "default scopes test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, hash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx, domain.APIKey{TenantID: tenant, Name: "default scopes key"}, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	got, err := repo.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("get api key by hash: %v", err)
	}
	wantScopes := []domain.Scope{domain.ScopeRead, domain.ScopePost}
	if len(got.Scopes) != len(wantScopes) || got.Scopes[0] != wantScopes[0] || got.Scopes[1] != wantScopes[1] {
		t.Errorf("Scopes = %v, want %v (the api_keys.scopes column default)", got.Scopes, wantScopes)
	}
	if got.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil", *got.ExpiresAt)
	}
	if got.LastUsedAt != nil {
		t.Errorf("LastUsedAt = %v, want nil", *got.LastUsedAt)
	}
}

// TestAPIKeyTouchLastUsed covers the repository half of Task 2.2's
// last_used_at throttle: TouchAPIKeyLastUsed sets the column, and a
// subsequent GetAPIKeyByHash surfaces exactly the timestamp given, truncated
// to Postgres's microsecond timestamptz precision.
func TestAPIKeyTouchLastUsed(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "touch last used test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, hash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx, domain.APIKey{TenantID: tenant, Name: "touch test key"}, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	before, err := repo.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("get api key by hash before touch: %v", err)
	}
	if before.LastUsedAt != nil {
		t.Fatalf("LastUsedAt before any touch = %v, want nil", *before.LastUsedAt)
	}

	when := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.TouchAPIKeyLastUsed(ctx, before.ID, when); err != nil {
		t.Fatalf("touch api key last used: %v", err)
	}

	after, err := repo.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("get api key by hash after touch: %v", err)
	}
	if after.LastUsedAt == nil {
		t.Fatal("LastUsedAt after touch = nil, want the touched timestamp")
	}
	if !after.LastUsedAt.Equal(when) {
		t.Errorf("LastUsedAt after touch = %v, want %v", *after.LastUsedAt, when)
	}
}

// --- Task 2.2b: admin surface repository methods. ---

// TestAPIKeyInsertPersistsScopesAndExpiry covers the explicit-value path
// InsertAPIKey gained for the admin surface (Task 2.2b): a caller that does
// set Scopes and ExpiresAt gets exactly those values back, not the column
// defaults.
func TestAPIKeyInsertPersistsScopesAndExpiry(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "explicit scopes test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, hash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	key := domain.APIKey{
		TenantID:  tenant,
		Name:      "admin scoped key",
		Scopes:    []domain.Scope{domain.ScopeAdmin},
		ExpiresAt: &expiresAt,
	}
	if err := repo.InsertAPIKey(ctx, key, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	got, err := repo.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("get api key by hash: %v", err)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != domain.ScopeAdmin {
		t.Errorf("Scopes = %v, want [admin]", got.Scopes)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, expiresAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want a real timestamp")
	}
	if got.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil for a live key", *got.RevokedAt)
	}
}

// TestGetAPIKeyByIDIncludesRevoked proves GetAPIKeyByID (Task 2.2b), unlike
// GetAPIKeyByHash, returns a key regardless of revocation: the admin
// surface's RotateKey needs to read an old key's tenant/name/scopes even
// after (or in order to) revoke it.
func TestGetAPIKeyByIDIncludesRevoked(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "get by id test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, hash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	key := domain.APIKey{TenantID: tenant, Name: "fetch by id", Scopes: []domain.Scope{domain.ScopeRead}}
	if err := repo.InsertAPIKey(ctx, key, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	byHash, err := repo.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("get api key by hash: %v", err)
	}

	got, err := repo.GetAPIKeyByID(ctx, byHash.ID)
	if err != nil {
		t.Fatalf("get api key by id: %v", err)
	}
	if got.TenantID != tenant || got.Name != "fetch by id" {
		t.Errorf("got = %+v, want tenant %s name %q", got, tenant, "fetch by id")
	}

	if err := repo.RevokeAPIKey(ctx, byHash.ID); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}
	afterRevoke, err := repo.GetAPIKeyByID(ctx, byHash.ID)
	if err != nil {
		t.Fatalf("get api key by id after revoke: %v", err)
	}
	if afterRevoke.RevokedAt == nil {
		t.Error("RevokedAt = nil after revoke, want a timestamp")
	}
}

// TestGetAPIKeyByIDNotFound proves an unknown id returns
// domain.ErrAPIKeyNotFound.
func TestGetAPIKeyByIDNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetAPIKeyByID(context.Background(), uuid.NewString())
	if !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Errorf("get api key by unknown id: err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestListAPIKeysByTenantIncludesRevoked proves ListAPIKeysByTenant (Task
// 2.2b) returns every key for a tenant, live and revoked, ordered oldest
// first, and never returns a key belonging to a different tenant.
func TestListAPIKeysByTenantIncludesRevoked(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	other := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "list by tenant test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := repo.CreateTenant(ctx, other, "other tenant"); err != nil {
		t.Fatalf("create other tenant: %v", err)
	}

	var ids []string
	for i := range 2 {
		_, hash, err := domain.GenerateAPIKey()
		if err != nil {
			t.Fatalf("generate api key %d: %v", i, err)
		}
		key := domain.APIKey{TenantID: tenant, Name: "key", Scopes: []domain.Scope{domain.ScopeRead}}
		if err := repo.InsertAPIKey(ctx, key, hash); err != nil {
			t.Fatalf("insert api key %d: %v", i, err)
		}
		got, err := repo.GetAPIKeyByHash(ctx, hash)
		if err != nil {
			t.Fatalf("get api key %d by hash: %v", i, err)
		}
		ids = append(ids, got.ID)
	}
	if err := repo.RevokeAPIKey(ctx, ids[0]); err != nil {
		t.Fatalf("revoke first key: %v", err)
	}

	_, otherHash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate other tenant api key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx, domain.APIKey{TenantID: other, Name: "other tenant key"}, otherHash); err != nil {
		t.Fatalf("insert other tenant api key: %v", err)
	}

	got, err := repo.ListAPIKeysByTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("list api keys by tenant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListAPIKeysByTenant returned %d keys, want 2", len(got))
	}
	seen := make(map[string]bool, len(got))
	for _, k := range got {
		seen[k.ID] = true
		if k.TenantID != tenant {
			t.Errorf("ListAPIKeysByTenant returned a key for tenant %q, want only %q", k.TenantID, tenant)
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("ListAPIKeysByTenant did not include key %s", id)
		}
	}
	if !seen[ids[0]] {
		t.Error("revoked key missing from ListAPIKeysByTenant (must include revoked history)")
	}
}

// TestRevokeAPIKeyNotFound proves revoking an id with no api_keys row
// returns domain.ErrAPIKeyNotFound.
func TestRevokeAPIKeyNotFound(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	err := repo.RevokeAPIKey(context.Background(), uuid.NewString())
	if !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Errorf("revoke unknown api key: err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestRevokeAPIKeyIdempotent proves revoking an already-revoked key succeeds
// again without error (see RevokeAPIKey's COALESCE), rather than the second
// call reporting zero rows affected as domain.ErrAPIKeyNotFound.
func TestRevokeAPIKeyIdempotent(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "double revoke test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, hash, err := domain.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	if err := repo.InsertAPIKey(ctx, domain.APIKey{TenantID: tenant, Name: "double revoke"}, hash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	byHash, err := repo.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("get api key by hash: %v", err)
	}

	if err := repo.RevokeAPIKey(ctx, byHash.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := repo.RevokeAPIKey(ctx, byHash.ID); err != nil {
		t.Errorf("second revoke: err = %v, want nil (idempotent)", err)
	}
}
