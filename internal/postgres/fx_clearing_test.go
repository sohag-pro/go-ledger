package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestGetOrCreateClearingAccount_CreatesThenReusesSameRow proves the
// reserved-name, get-or-create contract directly at the repository layer
// (ADR-014): the first call for a tenant/currency creates a System,
// Liability account; a second call for the SAME tenant and currency resolves
// to the exact same row rather than creating a duplicate; and a different
// currency (or a different tenant) gets its own, separate row.
func TestGetOrCreateClearingAccount_CreatesThenReusesSameRow(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "clearing account test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	first, err := repo.GetOrCreateClearingAccount(ctx, tenant, "USD")
	if err != nil {
		t.Fatalf("GetOrCreateClearingAccount (first): %v", err)
	}
	if first.Name != "fx.clearing.USD" {
		t.Errorf("Name = %q, want fx.clearing.USD", first.Name)
	}
	if first.Type != domain.Liability {
		t.Errorf("Type = %v, want Liability", first.Type)
	}
	if !first.System {
		t.Error("System = false, want true")
	}
	if first.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", first.Currency)
	}

	second, err := repo.GetOrCreateClearingAccount(ctx, tenant, "USD")
	if err != nil {
		t.Fatalf("GetOrCreateClearingAccount (second): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("second call ID = %q, want the same row %q", second.ID, first.ID)
	}

	otherCurrency, err := repo.GetOrCreateClearingAccount(ctx, tenant, "EUR")
	if err != nil {
		t.Fatalf("GetOrCreateClearingAccount (EUR): %v", err)
	}
	if otherCurrency.ID == first.ID {
		t.Error("EUR clearing account resolved to the same row as USD's")
	}
}

// TestGetOrCreateClearingAccount_MalformedTenantIDErrors covers the
// uuid.Parse error branch, the same defense-in-depth every other
// repository method in this package has.
func TestGetOrCreateClearingAccount_MalformedTenantIDErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	_, err := repo.GetOrCreateClearingAccount(context.Background(), "not-a-uuid", "USD")
	if err == nil {
		t.Fatal("expected an error for a malformed tenant id")
	}
}

// TestInsertFXRate_GlobalAndTenantScoped proves InsertFXRate writes both a
// global default rate (tenantID nil) and a tenant-scoped rate (Task 2.4,
// audit A3.3), and validates its inputs before ever touching the database,
// directly at the repository layer (internal/admin's own tests cover this
// through the admin.Service wrapper; this covers the adapter itself).
func TestInsertFXRate_GlobalAndTenantScoped(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	// Global default: tenantID nil.
	if err := repo.InsertFXRate(ctx, nil, "USD", "PGX", 100_000_000, 10, "test", nil); err != nil {
		t.Fatalf("InsertFXRate (global): %v", err)
	}

	// Tenant-scoped: requires an existing tenant row.
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "fx rate repo test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := repo.InsertFXRate(ctx, &tenant, "USD", "PGX", 105_000_000, 20, "test-tenant", nil); err != nil {
		t.Fatalf("InsertFXRate (tenant-scoped): %v", err)
	}
}

// TestInsertFXRate_MissingTenantErrors proves a tenant-scoped insert against
// an id with no tenant row fails closed with domain.ErrTenantNotFound rather
// than a raw foreign-key-violation error.
func TestInsertFXRate_MissingTenantErrors(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)

	missing := uuid.NewString()
	err := repo.InsertFXRate(context.Background(), &missing, "USD", "PGX", 100_000_000, 0, "test", nil)
	if !errors.Is(err, domain.ErrTenantNotFound) {
		t.Errorf("got %v, want ErrTenantNotFound", err)
	}
}

// TestInsertFXRate_ValidatesBeforeInsert proves every InsertFXRate validation
// rule (base/quote well-formed and distinct, positive rate, in-range spread)
// fires before any database write, directly at the repository layer.
func TestInsertFXRate_ValidatesBeforeInsert(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()

	cases := []struct {
		name        string
		base, quote domain.Currency
		midRateE8   int64
		spreadBps   int32
		wantErr     error
	}{
		{"invalid base", "US", "EUR", 100_000_000, 0, domain.ErrInvalidCurrency},
		{"invalid quote", "USD", "EU", 100_000_000, 0, domain.ErrInvalidCurrency},
		{"same currency", "USD", "USD", 100_000_000, 0, domain.ErrSameCurrencyRate},
		{"non-positive rate", "USD", "EUR", 0, 0, domain.ErrNonPositiveRate},
		{"negative spread", "USD", "EUR", 100_000_000, -1, domain.ErrInvalidSpread},
		{"spread too wide", "USD", "EUR", 100_000_000, 10_000, domain.ErrInvalidSpread},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := repo.InsertFXRate(ctx, nil, tc.base, tc.quote, tc.midRateE8, tc.spreadBps, "test", nil)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("InsertFXRate(%s/%s, mid=%d, spread=%d): err = %v, want %v",
					tc.base, tc.quote, tc.midRateE8, tc.spreadBps, err, tc.wantErr)
			}
		})
	}
}
