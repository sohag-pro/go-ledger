package fx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
)

// TestFeed_RefreshWritesAndDedups proves the live feed fetches from a
// (mocked) Frankfurter endpoint, appends a fresh global row per configured
// currency, and skips a second refresh when the rate is unchanged (audit A:
// no FX rate staleness guard, the fresh-rate half).
func TestFeed_RefreshWritesAndDedups(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	// A tenant with no rates of its own resolves the global default the feed writes.
	tenant := newTestTenant(t, pool)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"base":"USD","rates":{"EUR":0.92,"GBP":0.79}}`))
	}))
	defer srv.Close()

	feed := fx.NewFeed(pool, nil, fx.FeedConfig{
		URL:        srv.URL,
		Base:       domain.Currency("USD"),
		Currencies: []domain.Currency{"EUR", "GBP"},
	})

	// The test DB is shared, so other tests' global rows may pre-exist; assert
	// on the resolved values rather than the exact write count.
	if _, err := feed.RefreshOnce(ctx); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	// Both fed rates now resolve as the fresh global default.
	provider := fx.NewDBProvider(pool, fx.WithMaxRateAge(24*time.Hour))
	for _, tc := range []struct {
		quote string
		mid   int64
	}{
		{"EUR", 92_000_000}, // 0.92 * 1e8
		{"GBP", 79_000_000}, // 0.79 * 1e8
	} {
		q, _, err := provider.Rate(ctx, tenant, domain.Currency("USD"), domain.Currency(tc.quote))
		if err != nil {
			t.Fatalf("Rate(USD, %s) after feed: %v", tc.quote, err)
		}
		if q.MidRateE8 != tc.mid {
			t.Errorf("fed USD/%s mid = %d, want %d", tc.quote, q.MidRateE8, tc.mid)
		}
	}

	// A second refresh with unchanged rates writes nothing (dedup): both pairs
	// now match the current stored mid.
	n, err := feed.RefreshOnce(ctx)
	if err != nil {
		t.Fatalf("second RefreshOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("second refresh wrote %d rows, want 0 (unchanged rates dedup)", n)
	}
}

// TestFeed_NoCurrenciesIsNoop proves an enabled feed with no configured
// currencies makes no network call and writes nothing.
func TestFeed_NoCurrenciesIsNoop(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	feed := fx.NewFeed(pool, nil, fx.FeedConfig{URL: srv.URL, Base: domain.Currency("USD")})
	n, err := feed.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}
	if n != 0 || called {
		t.Errorf("no-currency refresh: wrote=%d called=%v, want 0/false", n, called)
	}
}
