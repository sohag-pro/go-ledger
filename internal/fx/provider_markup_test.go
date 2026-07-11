package fx_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// insertRateRaw writes an fx_rates row with an explicit spread_bps validity,
// so markup-precedence tests can seed either a per-pair override (Valid:
// true) or a row that falls through to the markup default (Valid: false),
// unlike insertRate/insertTenantRate above which always insert a valid
// spread.
func insertRateRaw(t *testing.T, q *sqlc.Queries, tenantID pgtype.UUID, base, quote string, midE8 int64, spread pgtype.Int4) {
	t.Helper()
	if _, err := q.InsertFXRate(context.Background(), sqlc.InsertFXRateParams{
		TenantID:    tenantID,
		Base:        base,
		Quote:       quote,
		MidRateE8:   midE8,
		SpreadBps:   spread,
		Source:      "test",
		EffectiveAt: pgtype.Timestamptz{Time: time.Now().UTC().Add(-2 * time.Second), Valid: true},
	}); err != nil {
		t.Fatalf("insert fx rate %s/%s: %v", base, quote, err)
	}
}

// insertMarkupDefault writes an fx_markup_defaults row, global when tenantID
// is not Valid, tenant-owned otherwise. defaultSpreadBps is a raw pgtype.Int4
// so a case can write either an explicit value (Valid: true) or a CLEAR
// (pgtype.Int4{}, i.e. Valid: false), the same way insertRateRaw above lets a
// case write either a spread override or a NULL. Unlike insertRateRaw, it
// does not backdate effective_at: fx_markup_defaults' global (tenant_id
// NULL) row has no per-pair partition (unlike fx_rates), so its "current"
// row is whatever is latest across the whole shared table, full stop. A
// fixed backdate margin can invert that ordering against a real
// (non-backdated) row written moments later elsewhere in the suite (for
// example AdminService, which deliberately lets the server stamp
// effective_at for the same clock-skew reason documented on InsertFXRate),
// so this leaves effective_at unset and lets the server's own now() win the
// same way.
func insertMarkupDefault(t *testing.T, q *sqlc.Queries, tenantID pgtype.UUID, defaultSpreadBps pgtype.Int4) {
	t.Helper()
	if _, err := q.InsertFXMarkupDefault(context.Background(), sqlc.InsertFXMarkupDefaultParams{
		TenantID:         tenantID,
		DefaultSpreadBps: defaultSpreadBps,
		Source:           "test",
	}); err != nil {
		t.Fatalf("insert fx markup default: %v", err)
	}
}

// validSpread wraps v as a Valid pgtype.Int4, for the common case of
// insertMarkupDefault writing an explicit (not cleared) value.
func validSpread(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }

// TestProviderResolvesMarkupPrecedence covers the ADR-020 precedence chain a
// conversion resolves its spread through: a per-pair override on the rate row
// wins outright; absent that, the tenant's own markup default; absent that,
// the global default; absent that, zero. Each case seeds its own disjoint
// currency pair and a fresh tenant, so cases never interfere with each other
// even though fx_rates and fx_markup_defaults are both shared, package-wide
// tables.
func TestProviderResolvesMarkupPrecedence(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	provider := fx.NewDBProvider(pool)

	// "No default anywhere" (a NULL rate spread resolving to zero rather than
	// erroring, when neither a tenant nor a global markup default exists) is
	// asserted once here from noDefaultSpreadBps/noDefaultErr, captured in
	// TestMain (see probeNoDefaultAnywhere in provider_test.go) before any
	// test could write to fx_markup_defaults. That table is shared,
	// package-wide, and not partitioned by currency pair: whatever the most
	// recently inserted global default is wins for every pair and tenant, so
	// "nothing configured anywhere" can only ever be true once, at the very
	// start of the process, before this test (and every other case in its own
	// table below, which deliberately DO write global and tenant defaults)
	// gets a chance to run.
	if noDefaultErr != nil {
		t.Fatalf("probeNoDefaultAnywhere() error = %v", noDefaultErr)
	}
	if noDefaultSpreadBps != 0 {
		t.Errorf("probeNoDefaultAnywhere() spreadBps = %d, want 0 (no default configured anywhere yet)", noDefaultSpreadBps)
	}

	// Every case's global insert (where present) uses the same value, 10, so
	// their expectations stay correct regardless of exactly which global row
	// CurrentFXMarkupDefault actually picks among them.
	tests := []struct {
		name string
		// setup seeds fx_rates and/or fx_markup_defaults for the given tenant
		// and currency pair, then returns the (base, quote) Rate should be
		// asked for.
		setup func(t *testing.T, tenantID string) (base, quote domain.Currency)
		want  int32
	}{
		{
			// A rate row with an explicit spread wins outright, even if a
			// tenant and global default also exist.
			name: "explicit override wins",
			setup: func(t *testing.T, tenantID string) (domain.Currency, domain.Currency) {
				tid := parseTenant(t, tenantID)
				insertRateRaw(t, q, tid, "ABC", "DEF", 100_000_000, pgtype.Int4{Int32: 25, Valid: true})
				insertMarkupDefault(t, q, tid, validSpread(40))
				insertMarkupDefault(t, q, pgtype.UUID{}, validSpread(10))
				return "ABC", "DEF"
			},
			want: 25,
		},
		{
			// NULL spread, only a global default exists: the global default
			// wins.
			name: "global default when no tenant default",
			setup: func(t *testing.T, _ string) (domain.Currency, domain.Currency) {
				insertRateRaw(t, q, pgtype.UUID{}, "MNO", "PQR", 100_000_000, pgtype.Int4{})
				insertMarkupDefault(t, q, pgtype.UUID{}, validSpread(10))
				return "MNO", "PQR"
			},
			want: 10,
		},
		{
			// NULL spread, a tenant default and a global default both exist:
			// the tenant default wins.
			name: "tenant default wins over global",
			setup: func(t *testing.T, tenantID string) (domain.Currency, domain.Currency) {
				tid := parseTenant(t, tenantID)
				insertRateRaw(t, q, pgtype.UUID{}, "GHI", "JKL", 100_000_000, pgtype.Int4{})
				insertMarkupDefault(t, q, tid, validSpread(40))
				insertMarkupDefault(t, q, pgtype.UUID{}, validSpread(10))
				return "GHI", "JKL"
			},
			want: 40,
		},
		{
			// The inverse-derived pair (only the reverse direction is
			// stored) still resolves the tenant's markup default through
			// the same precedence chain.
			name: "inverse pair resolves tenant default",
			setup: func(t *testing.T, tenantID string) (domain.Currency, domain.Currency) {
				tid := parseTenant(t, tenantID)
				// Store YZA/BCD; the test requests the inverse BCD/YZA.
				insertRateRaw(t, q, pgtype.UUID{}, "YZA", "BCD", 150_000_000, pgtype.Int4{})
				insertMarkupDefault(t, q, tid, validSpread(40))
				return "BCD", "YZA"
			},
			want: 40,
		},
		{
			// A tenant default exists, but the tenant's LATEST row is a
			// clear (default_spread_bps NULL, ADR-020/migration 0031's
			// fix): resolution must fall through to the global default, not
			// treat the clear as an explicit zero or keep using the
			// tenant's earlier (now-superseded) value.
			name: "cleared tenant default falls back to global",
			setup: func(t *testing.T, tenantID string) (domain.Currency, domain.Currency) {
				tid := parseTenant(t, tenantID)
				insertRateRaw(t, q, pgtype.UUID{}, "STU", "TUV", 100_000_000, pgtype.Int4{})
				insertMarkupDefault(t, q, tid, validSpread(40)) // tenant sets an override
				insertMarkupDefault(t, q, tid, pgtype.Int4{})   // tenant clears it (NULL)
				insertMarkupDefault(t, q, pgtype.UUID{}, validSpread(10))
				return "STU", "TUV"
			},
			want: 10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := newTestTenant(t, pool)
			base, quote := tc.setup(t, tenantID)

			_, spreadBps, err := provider.Rate(ctx, tenantID, base, quote)
			if err != nil {
				t.Fatalf("Rate(%s, %s/%s) error = %v", tenantID, base, quote, err)
			}
			if spreadBps != tc.want {
				t.Errorf("Rate(%s, %s/%s) spreadBps = %d, want %d", tenantID, base, quote, spreadBps, tc.want)
			}
		})
	}
}

// parseTenant parses a tenant id string into a valid pgtype.UUID, for seeding
// a tenant-scoped fx_rates or fx_markup_defaults row directly via sqlc.
func parseTenant(t *testing.T, tenantID string) pgtype.UUID {
	t.Helper()
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		t.Fatalf("parse tenant id %q: %v", tenantID, err)
	}
	return pgtype.UUID{Bytes: tid, Valid: true}
}

// TestListCurrentFXRates covers ListCurrentFXRates's DISTINCT ON collapse: one
// row per (base, quote) pair, with a tenant-owned row taking precedence over
// a global one for the same pair, matching CurrentFXRate's own precedence.
// Two pairs are seeded, each disjoint from every other test in this package
// (fx_rates is a shared, package-wide table): one with only a global rate,
// one with both a global and a tenant-specific rate for the tenant under
// test.
func TestListCurrentFXRates(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	tenantID := newTestTenant(t, pool)
	tid := parseTenant(t, tenantID)

	// Global-only pair: no tenant-specific row exists for it, so the global
	// rate is the only candidate and must be the one returned.
	insertRateRaw(t, q, pgtype.UUID{}, "PQZ", "XYQ", 100_000_000, pgtype.Int4{Int32: 15, Valid: true})

	// Contested pair: both a global row and this tenant's own row exist. The
	// tenant's row carries a different mid so the test can tell which one
	// ListCurrentFXRates actually returned.
	insertRateRaw(t, q, pgtype.UUID{}, "HJK", "LMN", 200_000_000, pgtype.Int4{Int32: 20, Valid: true})
	insertRateRaw(t, q, tid, "HJK", "LMN", 250_000_000, pgtype.Int4{Int32: 30, Valid: true})

	rows, err := q.ListCurrentFXRates(ctx, tid)
	if err != nil {
		t.Fatalf("ListCurrentFXRates(%s) error = %v", tenantID, err)
	}

	got := make(map[string]sqlc.ListCurrentFXRatesRow, len(rows))
	for _, r := range rows {
		key := r.Base + "/" + r.Quote
		if _, dup := got[key]; dup {
			t.Fatalf("ListCurrentFXRates(%s) returned more than one row for %s", tenantID, key)
		}
		got[key] = r
	}

	globalOnly, ok := got["PQZ/XYQ"]
	if !ok {
		t.Fatalf("ListCurrentFXRates(%s) missing PQZ/XYQ", tenantID)
	}
	if globalOnly.MidRateE8 != 100_000_000 {
		t.Errorf("PQZ/XYQ MidRateE8 = %d, want %d", globalOnly.MidRateE8, 100_000_000)
	}
	if globalOnly.TenantID.Valid {
		t.Errorf("PQZ/XYQ TenantID = %v, want the global (invalid/NULL) row", globalOnly.TenantID)
	}

	contested, ok := got["HJK/LMN"]
	if !ok {
		t.Fatalf("ListCurrentFXRates(%s) missing HJK/LMN", tenantID)
	}
	if contested.MidRateE8 != 250_000_000 {
		t.Errorf("HJK/LMN MidRateE8 = %d, want %d (the tenant-specific row)", contested.MidRateE8, 250_000_000)
	}
	if !contested.TenantID.Valid || contested.TenantID.Bytes != tid.Bytes {
		t.Errorf("HJK/LMN TenantID = %v, want tenant %s", contested.TenantID, tenantID)
	}
}
