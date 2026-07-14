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

// TestProviderTriangulatesViaUSD proves a cross pair with no direct or inverse
// rate is priced through the USD hub (ADR-022), composing the two USD legs, and
// that the markup is the SUM of both legs' spreads (audit A: hub spread
// under-applied). A two-hop conversion crosses two markups, so charging only
// one leg's spread underpriced the cross vs the equivalent pair of direct
// conversions and left an arbitrage.
func TestProviderTriangulatesViaUSD(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	tenant := newTestTenant(t, pool)
	now := time.Now()

	// 1 USD = 0.50 EUR, markup 30 bps on USD/EUR (the EUR->USD base leg).
	insertTenantRate(t, q, tenant, "USD", "EUR", 50_000_000, 30, now)
	// 1 USD = 100 BDT, markup 99 bps on the USD->BDT leg.
	insertTenantRate(t, q, tenant, "USD", "BDT", 10_000_000_000, 99, now)

	provider := fx.NewDBProvider(pool)

	// 1 EUR = 2 USD = 200 BDT, so the composed mid is 200.00 -> 20_000_000_000.
	quote, spread, err := provider.Rate(ctx, tenant, domain.Currency("EUR"), domain.Currency("BDT"))
	if err != nil {
		t.Fatalf("Rate(EUR, BDT) via USD hub: %v", err)
	}
	if quote.MidRateE8 != 20_000_000_000 {
		t.Errorf("hub mid = %d, want 20000000000 (200.00 BDT per EUR)", quote.MidRateE8)
	}
	if spread != 129 {
		t.Errorf("hub spread = %d, want 129 (both legs: EUR->USD 30 + USD->BDT 99)", spread)
	}

	// A cross with a missing leg (no MYR rate anywhere) still fails closed.
	_, _, err = provider.Rate(ctx, tenant, domain.Currency("EUR"), domain.Currency("MYR"))
	if !errors.Is(err, domain.ErrFXRateNotFound) {
		t.Errorf("Rate(EUR, MYR) with no MYR leg: err = %v, want ErrFXRateNotFound", err)
	}
}
