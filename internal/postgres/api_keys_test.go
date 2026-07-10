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

// TestAPIKeyDefaultScopesAndNullLifecycleFields covers Task 2.2's schema
// defaults: InsertAPIKey does not accept Scopes, ExpiresAt, or LastUsedAt
// (the admin surface that sets them is a separate follow-up task), so every
// key it inserts picks up the api_keys.scopes column default ({read,post})
// and NULL expires_at/last_used_at, and GetAPIKeyByHash must surface exactly
// that: a pre-2.2-shaped key keeps working unchanged.
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
