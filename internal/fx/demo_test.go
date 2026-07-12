package fx_test

import (
	"context"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/fx"
)

// TestPrefillDemoRates proves the demo prefill writes the four USD-based pairs
// with no per-pair spread (so the 1 percent tenant markup applies to each) and
// a 100 bps tenant markup default.
func TestPrefillDemoRates(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	svc := fx.NewAdminService(pool)
	tenant := newTestTenant(t, pool)

	if err := fx.PrefillDemoRates(ctx, svc, tenant); err != nil {
		t.Fatalf("PrefillDemoRates: %v", err)
	}

	rates, err := svc.ListRates(ctx, tenant)
	if err != nil {
		t.Fatalf("ListRates: %v", err)
	}
	want := []struct {
		base, quote string
		mid         int64
	}{
		{"USD", "EUR", 87549400},
		{"USD", "MYR", 407046600},
		{"USD", "BDT", 12327355800},
		{"USD", "INR", 9554823100},
	}
	for _, w := range want {
		v, ok := findRateView(rates, w.base, w.quote)
		if !ok {
			t.Fatalf("ListRates missing prefilled %s/%s", w.base, w.quote)
		}
		if v.MidRateE8 != w.mid {
			t.Errorf("%s/%s mid = %d, want %d", w.base, w.quote, v.MidRateE8, w.mid)
		}
		if v.SpreadBps != nil {
			t.Errorf("%s/%s has a per-pair spread %d, want nil so the markup default applies", w.base, w.quote, *v.SpreadBps)
		}
		if v.EffectiveSpreadBps != 100 {
			t.Errorf("%s/%s effective spread = %d, want 100 (the 1 percent tenant markup)", w.base, w.quote, v.EffectiveSpreadBps)
		}
	}

	mv, err := svc.GetMarkup(ctx, tenant)
	if err != nil {
		t.Fatalf("GetMarkup: %v", err)
	}
	if mv.Tenant == nil || mv.Tenant.DefaultSpreadBps == nil || *mv.Tenant.DefaultSpreadBps != 100 {
		t.Fatalf("tenant markup = %+v, want a 100 bps default", mv.Tenant)
	}
}
