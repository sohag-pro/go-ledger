package fx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sohag-pro/go-ledger/internal/fx"
)

// int32Ptr returns a pointer to v, for building a spread_bps override in
// tests without a named local variable at every call site.
func int32Ptr(v int32) *int32 { return &v }

// findRateView returns the row for (base, quote) out of a ListRates result,
// so assertions can address one pair without depending on slice order.
func findRateView(rates []fx.RateView, base, quote string) (fx.RateView, bool) {
	for _, r := range rates {
		if r.Base == base && r.Quote == quote {
			return r, true
		}
	}
	return fx.RateView{}, false
}

// TestAdminServiceInsertAndList covers the AdminService write/read surface
// end to end against a live database: appending a rate (and a later
// override for the same pair), listing the current effective rate per pair
// with its resolved spread, appending a markup default, and the effective
// spread falling back to that markup default when a rate row carries no
// override of its own. It uses its own currency pairs (ZQX/ZQY, ZQX/ZQZ),
// disjoint from every other test in this package (fx_rates and
// fx_markup_defaults are shared, package-wide tables), so it never races
// another test's assertions on the same rows.
//
// It is deliberately not t.Parallel(): it is the only test in this package
// that inserts a truly global (tenant_id NULL) markup default, and
// TestProviderResolvesMarkupPrecedence's "zero when no default anywhere"
// case (provider_markup_test.go) depends on no global default existing yet
// when it runs. Non-parallel tests all run to completion, in file order,
// before any parallel-marked test's body starts, so keeping this test
// non-parallel and doing its SetMarkup call only after several other steps
// (matching the order below) leaves that other test's window intact.
func TestAdminServiceInsertAndList(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	svc := fx.NewAdminService(pool)

	const base, quote, otherQuote = "ZQX", "ZQY", "ZQZ"

	inserted, err := svc.InsertRate(ctx, "", base, quote, 92_000_000, nil)
	if err != nil {
		t.Fatalf("InsertRate(%s/%s, mid 92000000, no spread) error = %v", base, quote, err)
	}
	if inserted.MidRateE8 != 92_000_000 {
		t.Errorf("InsertRate() MidRateE8 = %d, want 92000000", inserted.MidRateE8)
	}
	if inserted.SpreadBps != nil {
		t.Errorf("InsertRate() SpreadBps = %v, want nil", inserted.SpreadBps)
	}

	if _, err := svc.InsertRate(ctx, "", base, quote, 93_000_000, int32Ptr(25)); err != nil {
		t.Fatalf("InsertRate(%s/%s, mid 93000000, spread 25) error = %v", base, quote, err)
	}

	rates, err := svc.ListRates(ctx, "")
	if err != nil {
		t.Fatalf("ListRates() error = %v", err)
	}
	overridden, ok := findRateView(rates, base, quote)
	if !ok {
		t.Fatalf("ListRates() missing %s/%s", base, quote)
	}
	if overridden.MidRateE8 != 93_000_000 {
		t.Errorf("ListRates() %s/%s MidRateE8 = %d, want 93000000 (the newer override)", base, quote, overridden.MidRateE8)
	}
	if overridden.EffectiveSpreadBps != 25 {
		t.Errorf("ListRates() %s/%s EffectiveSpreadBps = %d, want 25", base, quote, overridden.EffectiveSpreadBps)
	}

	markup, err := svc.SetMarkup(ctx, "", int32Ptr(50))
	if err != nil {
		t.Fatalf("SetMarkup(global, 50) error = %v", err)
	}
	if markup.DefaultSpreadBps == nil || *markup.DefaultSpreadBps != 50 {
		t.Errorf("SetMarkup() DefaultSpreadBps = %v, want 50", markup.DefaultSpreadBps)
	}

	view, err := svc.GetMarkup(ctx, "")
	if err != nil {
		t.Fatalf("GetMarkup(global) error = %v", err)
	}
	if view.Global == nil {
		t.Fatalf("GetMarkup(global).Global = nil, want the row just set")
	}
	if view.Global.DefaultSpreadBps == nil || *view.Global.DefaultSpreadBps != 50 {
		t.Errorf("GetMarkup(global).Global.DefaultSpreadBps = %v, want 50", view.Global.DefaultSpreadBps)
	}

	if _, err := svc.InsertRate(ctx, "", base, otherQuote, 110_5000_0000, nil); err != nil {
		t.Fatalf("InsertRate(%s/%s) error = %v", base, otherQuote, err)
	}
	rates, err = svc.ListRates(ctx, "")
	if err != nil {
		t.Fatalf("ListRates() second call error = %v", err)
	}
	fallback, ok := findRateView(rates, base, otherQuote)
	if !ok {
		t.Fatalf("ListRates() missing %s/%s", base, otherQuote)
	}
	if fallback.SpreadBps != nil {
		t.Errorf("ListRates() %s/%s SpreadBps = %v, want nil (no override stored on the row)", base, otherQuote, fallback.SpreadBps)
	}
	if fallback.EffectiveSpreadBps != 50 {
		t.Errorf("ListRates() %s/%s EffectiveSpreadBps = %d, want 50 (falls back to the global markup default)",
			base, otherQuote, fallback.EffectiveSpreadBps)
	}

	invalidCases := []struct {
		name string
		run  func() error
	}{
		{
			name: "base equals quote",
			run: func() error {
				_, err := svc.InsertRate(ctx, "", "AAA", "AAA", 1_000_000, nil)
				return err
			},
		},
		{
			name: "non-positive mid rate",
			run: func() error {
				_, err := svc.InsertRate(ctx, "", "BBB", "CCC", 0, nil)
				return err
			},
		},
		{
			name: "negative mid rate",
			run: func() error {
				_, err := svc.InsertRate(ctx, "", "DDD", "EEE", -1, nil)
				return err
			},
		},
		{
			name: "spread at upper bound",
			run: func() error {
				_, err := svc.InsertRate(ctx, "", "FFF", "GGG", 1_000_000, int32Ptr(10000))
				return err
			},
		},
		{
			name: "negative spread",
			run: func() error {
				_, err := svc.InsertRate(ctx, "", "HHH", "III", 1_000_000, int32Ptr(-1))
				return err
			},
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, fx.ErrInvalidFXInput) {
				t.Errorf("%s: error = %v, want it to wrap fx.ErrInvalidFXInput", tc.name, err)
			}
		})
	}
}

// TestListRatesResolvesEffectiveSpreadAgainstRequestedScope guards the fix for
// the bug where ListRates resolved a row's effective spread against the
// WINNING ROW's own tenant_id instead of the requested scope. ListCurrentFXRates
// can return a global-fallback row (tenant_id NULL) for a tenant-scoped
// request when no tenant-specific rate row exists for that pair; resolving
// against that NULL tenant_id skips the tenant's own markup default entirely
// (CurrentFXMarkupDefault would only ever match the global row). Here the
// tenant has its own markup default (80) that must win over the global
// default (50), even though the only rate row for the pair is the global one.
// This is safe to run in parallel with the rest of the package: fx_rates uses
// a disjoint currency pair and fx_markup_defaults resolves a tenant-specific
// row ahead of any global row regardless of insertion order or timing (see
// CurrentFXMarkupDefault's ORDER BY), so concurrent global writes elsewhere
// in the suite cannot change this tenant's result.
func TestListRatesResolvesEffectiveSpreadAgainstRequestedScope(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	svc := fx.NewAdminService(pool)

	tenantID := newTestTenant(t, pool)
	const base, quote = "VWX", "VWY"

	// Only a global rate row for this pair: no tenant-specific row exists, so
	// ListCurrentFXRates must return the global-fallback row for a request
	// scoped to tenantID.
	if _, err := svc.InsertRate(ctx, "", base, quote, 100_000_000, nil); err != nil {
		t.Fatalf("InsertRate(global, %s/%s) error = %v", base, quote, err)
	}

	if _, err := svc.SetMarkup(ctx, tenantID, int32Ptr(80)); err != nil {
		t.Fatalf("SetMarkup(tenant, 80) error = %v", err)
	}
	if _, err := svc.SetMarkup(ctx, "", int32Ptr(50)); err != nil {
		t.Fatalf("SetMarkup(global, 50) error = %v", err)
	}

	rates, err := svc.ListRates(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListRates(%s) error = %v", tenantID, err)
	}
	got, ok := findRateView(rates, base, quote)
	if !ok {
		t.Fatalf("ListRates(%s) missing %s/%s", tenantID, base, quote)
	}
	// Confirm this really is exercising the precedence-breaking scenario: the
	// returned row must be the global fallback (empty TenantID), not a
	// tenant-owned row, or the assertion below would prove nothing.
	if got.TenantID != "" {
		t.Fatalf("ListRates(%s) %s/%s TenantID = %q, want \"\" (the global fallback row)", tenantID, base, quote, got.TenantID)
	}
	if got.EffectiveSpreadBps != 80 {
		t.Errorf("ListRates(%s) %s/%s EffectiveSpreadBps = %d, want 80 (the tenant's own markup default must win over the global default, even though the rate row itself is the global one)",
			tenantID, base, quote, got.EffectiveSpreadBps)
	}
}

// TestFXAdminUnknownTenantMapsToErrUnknownTenant covers mapFKErr: a write
// scoped to a tenant id that is syntactically valid but does not exist in the
// tenants table must fail the fx_rates/fx_markup_defaults foreign key and
// come back as fx.ErrUnknownTenant, not a raw pgconn error, so handlers can
// map it to 422 without inspecting Postgres error codes themselves.
func TestFXAdminUnknownTenantMapsToErrUnknownTenant(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	svc := fx.NewAdminService(pool)

	unknownTenant := uuid.NewString()

	t.Run("InsertRate", func(t *testing.T) {
		t.Parallel()
		_, err := svc.InsertRate(ctx, unknownTenant, "VWZ", "VWQ", 100_000_000, nil)
		if !errors.Is(err, fx.ErrUnknownTenant) {
			t.Errorf("InsertRate(unknown tenant) error = %v, want it to wrap fx.ErrUnknownTenant", err)
		}
	})

	t.Run("SetMarkup", func(t *testing.T) {
		t.Parallel()
		_, err := svc.SetMarkup(ctx, unknownTenant, int32Ptr(30))
		if !errors.Is(err, fx.ErrUnknownTenant) {
			t.Errorf("SetMarkup(unknown tenant) error = %v, want it to wrap fx.ErrUnknownTenant", err)
		}
	})
}

// TestSetMarkupClearFallsBackToGlobal covers the AdminService side of the
// fix for finding 2 (a tenant markup default could never be cleared): a
// tenant sets its own override, a conversion for that tenant uses it, the
// tenant then clears it (SetMarkup with a nil bps), and a conversion for
// that tenant must go back to following the global default exactly the way
// a tenant that never had an override of its own would, not keep using the
// stale override or silently resolve to zero. GetMarkup must also surface
// the cleared row distinctly from "no tenant row at all": Tenant is a
// present *MarkupDefault whose own DefaultSpreadBps is nil.
//
// fx_markup_defaults' global tier is a single, database-wide "latest row
// wins" value shared with every other test in this package, not partitioned
// by currency pair or tenant, so this test never writes to the global scope
// itself and never asserts an exact resolved number (either would race
// against other parallel tests writing their own global default, exactly
// the flake this test hit before this fix: comparing against a hardcoded
// global value raced with provider_markup_test.go's own global writes).
// Instead, it compares tenantA (whose override was just cleared) against a
// second, freshly created tenantB that never had an override at all, both
// resolving the SAME shared rate row moments apart: whatever the global
// tier happens to be at read time, a correctly cleared tenant must resolve
// to the identical value a no-override tenant does.
func TestSetMarkupClearFallsBackToGlobal(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	svc := fx.NewAdminService(pool)

	tenantA := newTestTenant(t, pool)
	tenantB := newTestTenant(t, pool)
	const base, quote = "CDG", "CDH"

	// One global (tenant_id NULL) rate row for the pair, spread NULL so its
	// effective spread always resolves through the markup-default chain;
	// both tenants see this same row via ListRates' global fallback, since
	// neither has a rate row of its own.
	if _, err := svc.InsertRate(ctx, "", base, quote, 100_000_000, nil); err != nil {
		t.Fatalf("InsertRate(global, %s/%s) error = %v", base, quote, err)
	}

	if _, err := svc.SetMarkup(ctx, tenantA, int32Ptr(80)); err != nil {
		t.Fatalf("SetMarkup(tenantA, 80) error = %v", err)
	}

	ratesA, err := svc.ListRates(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListRates(tenantA) error = %v", err)
	}
	before, ok := findRateView(ratesA, base, quote)
	if !ok {
		t.Fatalf("ListRates(tenantA) missing %s/%s before clear", base, quote)
	}
	if before.EffectiveSpreadBps != 80 {
		t.Fatalf("ListRates(tenantA) %s/%s EffectiveSpreadBps before clear = %d, want 80 (the tenant override)",
			base, quote, before.EffectiveSpreadBps)
	}

	// tenantA clears its own override.
	cleared, err := svc.SetMarkup(ctx, tenantA, nil)
	if err != nil {
		t.Fatalf("SetMarkup(tenantA, nil) error = %v", err)
	}
	if cleared.DefaultSpreadBps != nil {
		t.Errorf("SetMarkup(tenantA, nil) DefaultSpreadBps = %v, want nil", *cleared.DefaultSpreadBps)
	}

	// Read both tenants through the SAME REPEATABLE READ transaction, so
	// both queries see one consistent snapshot of the shared, package-wide
	// fx_markup_defaults global tier: two separate READ COMMITTED queries a
	// moment apart (the first cut of this test) is still wide enough for
	// another parallel test's own global write to land in between and flip
	// the "current" global row out from under the comparison, which is
	// exactly the flake this snapshot avoids.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin snapshot tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txSvc := fx.NewAdminService(tx)

	ratesA, err = txSvc.ListRates(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListRates(tenantA) error = %v after clear", err)
	}
	afterA, ok := findRateView(ratesA, base, quote)
	if !ok {
		t.Fatalf("ListRates(tenantA) missing %s/%s after clear", base, quote)
	}
	ratesB, err := txSvc.ListRates(ctx, tenantB)
	if err != nil {
		t.Fatalf("ListRates(tenantB) error = %v", err)
	}
	neverOverridden, ok := findRateView(ratesB, base, quote)
	if !ok {
		t.Fatalf("ListRates(tenantB) missing %s/%s", base, quote)
	}
	if afterA.EffectiveSpreadBps != neverOverridden.EffectiveSpreadBps {
		t.Errorf("tenantA EffectiveSpreadBps after clear = %d, tenantB (never overridden) EffectiveSpreadBps = %d, want them equal (a cleared tenant must resolve exactly like a tenant with no override of its own)",
			afterA.EffectiveSpreadBps, neverOverridden.EffectiveSpreadBps)
	}
	if afterA.EffectiveSpreadBps == 80 {
		t.Errorf("tenantA EffectiveSpreadBps after clear = 80, want it to have changed from the cleared override")
	}

	view, err := svc.GetMarkup(ctx, tenantA)
	if err != nil {
		t.Fatalf("GetMarkup(tenantA) error = %v", err)
	}
	if view.Tenant == nil {
		t.Fatalf("GetMarkup(tenantA).Tenant = nil, want a present row (the cleared row itself), not absent")
	}
	if view.Tenant.DefaultSpreadBps != nil {
		t.Errorf("GetMarkup(tenantA).Tenant.DefaultSpreadBps = %v, want nil (a cleared row)", *view.Tenant.DefaultSpreadBps)
	}
}
