package postgres_test

// Task 6.2 / ADR-018 (audit remediation): these tests drive
// internal/postgres/crypto_keys.go's four repository methods DIRECTLY,
// against a real Postgres, rather than indirectly through
// internal/crypto.Cipher (see internal/ledger/crypto_shredding_test.go for
// that level). Coverage is per package: only a test living in
// internal/postgres itself counts toward internal/postgres's own coverage,
// so these exist even though the ledger-level tests already exercise the
// same methods through a real Cipher.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestCryptoKeys_MintCurrentAndVersionLookup covers the mint-then-read happy
// path: a brand-new tenant has no current DEK, minting version 1 stores the
// caller-supplied wrapped bytes verbatim and reports it live (not shredded),
// and both CurrentTenantDEK and TenantDEKVersion(1) then see the same row.
// TenantDEKVersion for a version that was never minted reports found=false.
func TestCryptoKeys_MintCurrentAndVersionLookup(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "crypto keys repo test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// No key minted yet: CurrentTenantDEK reports not found, not an error.
	wrapped, version, shredded, found, err := repo.CurrentTenantDEK(ctx, tenant)
	if err != nil {
		t.Fatalf("CurrentTenantDEK before any mint: %v", err)
	}
	if found {
		t.Fatalf("CurrentTenantDEK before any mint: found = true, want false (wrapped=%v version=%d shredded=%v)", wrapped, version, shredded)
	}

	candidate := []byte("candidate-wrapped-dek-material-v1")
	minted, mintedShredded, err := repo.MintTenantDEKVersion(ctx, tenant, 1, candidate)
	if err != nil {
		t.Fatalf("MintTenantDEKVersion(1): %v", err)
	}
	if mintedShredded {
		t.Fatal("MintTenantDEKVersion(1) on a brand-new tenant reported shredded = true")
	}
	if string(minted) != string(candidate) {
		t.Errorf("MintTenantDEKVersion(1) returned %q, want the candidate %q", minted, candidate)
	}

	// CurrentTenantDEK now finds it.
	wrapped, version, shredded, found, err = repo.CurrentTenantDEK(ctx, tenant)
	if err != nil {
		t.Fatalf("CurrentTenantDEK after mint: %v", err)
	}
	if !found || shredded {
		t.Fatalf("CurrentTenantDEK after mint: found=%v shredded=%v, want found=true shredded=false", found, shredded)
	}
	if version != 1 {
		t.Errorf("CurrentTenantDEK version = %d, want 1", version)
	}
	if string(wrapped) != string(candidate) {
		t.Errorf("CurrentTenantDEK wrapped = %q, want %q", wrapped, candidate)
	}

	// TenantDEKVersion(1) sees the identical row.
	wrappedV1, shredV1, foundV1, err := repo.TenantDEKVersion(ctx, tenant, 1)
	if err != nil {
		t.Fatalf("TenantDEKVersion(1): %v", err)
	}
	if !foundV1 || shredV1 {
		t.Fatalf("TenantDEKVersion(1): found=%v shredded=%v, want found=true shredded=false", foundV1, shredV1)
	}
	if string(wrappedV1) != string(candidate) {
		t.Errorf("TenantDEKVersion(1) wrapped = %q, want %q", wrappedV1, candidate)
	}

	// A version that was never minted: found = false, no error.
	_, _, foundV2, err := repo.TenantDEKVersion(ctx, tenant, 2)
	if err != nil {
		t.Fatalf("TenantDEKVersion(2) (never minted): %v", err)
	}
	if foundV2 {
		t.Error("TenantDEKVersion(2) (never minted): found = true, want false")
	}
}

// TestCryptoKeys_MintSameVersionTwiceFirstCallerWins proves
// MintTenantDEKVersion's ON CONFLICT DO UPDATE is a "return the winning row"
// trick, not a real overwrite: minting the SAME (tenant, version) pair a
// second time with a DIFFERENT candidate still returns the FIRST candidate,
// so two racing first-use Encrypt calls converge on one DEK rather than
// silently swapping keys underneath each other.
func TestCryptoKeys_MintSameVersionTwiceFirstCallerWins(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "crypto keys mint race test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	first := []byte("first-candidate")
	second := []byte("second-candidate-should-be-discarded")

	got1, _, err := repo.MintTenantDEKVersion(ctx, tenant, 1, first)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	got2, _, err := repo.MintTenantDEKVersion(ctx, tenant, 1, second)
	if err != nil {
		t.Fatalf("second mint (same version): %v", err)
	}
	if string(got1) != string(first) {
		t.Fatalf("first mint returned %q, want %q", got1, first)
	}
	if string(got2) != string(first) {
		t.Errorf("second mint (same version, different candidate) returned %q, want the FIRST candidate %q (first caller wins)", got2, first)
	}
}

// TestCryptoKeys_ShredThenReadBothWaysRedacted proves ShredTenantCryptoKey
// destroys the tenant's CURRENT version: CurrentTenantDEK and
// TenantDEKVersion for that exact version both report shredded=true with a
// nil wrapped key, but found stays true (a shredded row is not the same as
// no row at all).
func TestCryptoKeys_ShredThenReadBothWaysRedacted(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "crypto keys shred test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, _, err := repo.MintTenantDEKVersion(ctx, tenant, 1, []byte("live-dek")); err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := repo.ShredTenantCryptoKey(ctx, tenant); err != nil {
		t.Fatalf("shred: %v", err)
	}

	wrapped, version, shredded, found, err := repo.CurrentTenantDEK(ctx, tenant)
	if err != nil {
		t.Fatalf("CurrentTenantDEK after shred: %v", err)
	}
	if !found {
		t.Fatal("CurrentTenantDEK after shred: found = false, want true (a shredded row still exists)")
	}
	if !shredded {
		t.Error("CurrentTenantDEK after shred: shredded = false, want true")
	}
	if wrapped != nil {
		t.Errorf("CurrentTenantDEK after shred: wrapped = %v, want nil", wrapped)
	}
	if version != 1 {
		t.Errorf("CurrentTenantDEK after shred: version = %d, want 1", version)
	}

	wrappedV1, shredV1, foundV1, err := repo.TenantDEKVersion(ctx, tenant, 1)
	if err != nil {
		t.Fatalf("TenantDEKVersion(1) after shred: %v", err)
	}
	if !foundV1 || !shredV1 || wrappedV1 != nil {
		t.Errorf("TenantDEKVersion(1) after shred = (wrapped=%v shredded=%v found=%v), want (nil, true, true)", wrappedV1, shredV1, foundV1)
	}

	// Shredding with no crypto_keys row at all yet still leaves a permanent
	// version-1 tombstone (migration 0028's GREATEST(...,1)), so a
	// never-encrypted tenant that gets shredded cannot silently mint a live
	// version 1 afterward.
	neverEncrypted := uuid.NewString()
	if err := repo.CreateTenant(ctx, neverEncrypted, "crypto keys shred never encrypted tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := repo.ShredTenantCryptoKey(ctx, neverEncrypted); err != nil {
		t.Fatalf("shred never-encrypted tenant: %v", err)
	}
	_, _, shreddedNever, foundNever, err := repo.CurrentTenantDEK(ctx, neverEncrypted)
	if err != nil {
		t.Fatalf("CurrentTenantDEK for never-encrypted, now-shredded tenant: %v", err)
	}
	if !foundNever || !shreddedNever {
		t.Errorf("CurrentTenantDEK for never-encrypted, now-shredded tenant: found=%v shredded=%v, want true, true", foundNever, shreddedNever)
	}
}

// TestCryptoKeys_MalformedTenantID proves every one of the four crypto_keys
// repository methods fails closed with a parse error for a syntactically
// invalid tenant id, rather than reaching the database, mirroring
// TestMalformedIDsReturnErrors (coverage_test.go) for the rest of the
// repository surface.
func TestCryptoKeys_MalformedTenantID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	const bad = "not-a-uuid"

	tests := []struct {
		name string
		call func() error
	}{
		{"CurrentTenantDEK", func() error {
			_, _, _, _, err := repo.CurrentTenantDEK(ctx, bad)
			return err
		}},
		{"MintTenantDEKVersion", func() error {
			_, _, err := repo.MintTenantDEKVersion(ctx, bad, 1, []byte("x"))
			return err
		}},
		{"TenantDEKVersion", func() error {
			_, _, _, err := repo.TenantDEKVersion(ctx, bad, 1)
			return err
		}},
		{"ShredTenantCryptoKey", func() error {
			return repo.ShredTenantCryptoKey(ctx, bad)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); err == nil {
				t.Fatal("expected a parse error, got nil")
			}
		})
	}
}
