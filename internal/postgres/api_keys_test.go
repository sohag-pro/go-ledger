package postgres_test

import (
	"context"
	"errors"
	"testing"

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
