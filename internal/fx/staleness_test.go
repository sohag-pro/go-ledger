package fx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// TestProvider_StalenessGuard proves WithMaxRateAge refuses a rate whose
// effective_at is older than the configured age (audit A: no FX rate staleness
// guard), and that a fresh rate, or a disabled guard, still resolves.
func TestProvider_StalenessGuard(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	tenant := newTestTenant(t, pool)

	// A rate effective two days ago.
	insertTenantRate(t, q, tenant, "USD", "EUR", 92_000_000, 10, time.Now().Add(-48*time.Hour))

	// Guard at 24h: the 48h-old rate is stale.
	guarded := fx.NewDBProvider(pool, fx.WithMaxRateAge(24*time.Hour))
	if _, _, err := guarded.Rate(ctx, tenant, domain.Currency("USD"), domain.Currency("EUR")); !errors.Is(err, domain.ErrFXRateStale) {
		t.Errorf("Rate with a 48h-old rate under a 24h guard: err = %v, want ErrFXRateStale", err)
	}

	// The inverse direction is priced off the same stale row, so it is stale too.
	if _, _, err := guarded.Rate(ctx, tenant, domain.Currency("EUR"), domain.Currency("USD")); !errors.Is(err, domain.ErrFXRateStale) {
		t.Errorf("inverse Rate off a stale row: err = %v, want ErrFXRateStale", err)
	}

	// A guard wide enough (72h) accepts the same rate.
	wide := fx.NewDBProvider(pool, fx.WithMaxRateAge(72*time.Hour))
	if _, _, err := wide.Rate(ctx, tenant, domain.Currency("USD"), domain.Currency("EUR")); err != nil {
		t.Errorf("Rate under a 72h guard for a 48h-old rate: err = %v, want nil", err)
	}

	// A disabled guard (the default) accepts any age.
	off := fx.NewDBProvider(pool)
	if _, _, err := off.Rate(ctx, tenant, domain.Currency("USD"), domain.Currency("EUR")); err != nil {
		t.Errorf("Rate with the guard disabled: err = %v, want nil", err)
	}
}

// TestProvider_HubStalenessUsesOlderLeg proves a USD-hub cross is only as fresh
// as its staler leg: a fresh base leg cannot mask a stale quote leg.
func TestProvider_HubStalenessUsesOlderLeg(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	tenant := newTestTenant(t, pool)

	insertTenantRate(t, q, tenant, "USD", "EUR", 50_000_000, 10, time.Now())                    // fresh
	insertTenantRate(t, q, tenant, "USD", "BDT", 10_000_000_000, 10, time.Now().Add(-48*time.Hour)) // stale

	guarded := fx.NewDBProvider(pool, fx.WithMaxRateAge(24*time.Hour))
	if _, _, err := guarded.Rate(ctx, tenant, domain.Currency("EUR"), domain.Currency("BDT")); !errors.Is(err, domain.ErrFXRateStale) {
		t.Errorf("hub cross with one stale leg: err = %v, want ErrFXRateStale", err)
	}
}
