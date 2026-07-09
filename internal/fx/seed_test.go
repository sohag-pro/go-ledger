package fx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// rowCount returns how many fx_rates rows exist for a pair, for asserting
// append (row count growing) versus a guarded no-op (row count unchanged).
func rowCount(t *testing.T, base, quote string) int {
	t.Helper()
	pool := newTestPool(t)
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM fx_rates WHERE base = $1 AND quote = $2`, base, quote).Scan(&count); err != nil {
		t.Fatalf("count fx_rates rows for %s/%s: %v", base, quote, err)
	}
	return count
}

// TestSeed_ParsesExactValues is the parsing exactness check the task
// requires: "110.50" must land as exactly 11050000000 (110.50 *
// domain.RateScale), "0.9200" as exactly 92000000, with no rounding error
// of any kind, because the whole parse never goes through anything but
// string splitting and integer conversion.
func TestSeed_ParsesExactValues(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	if err := fx.Seed(ctx, pool, "USD:EUR=0.9200/25,USD:BDT=110.50/50"); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	eur, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "USD", Quote: "EUR"})
	if err != nil {
		t.Fatalf("CurrentFXRate(USD, EUR) error = %v", err)
	}
	if eur.MidRateE8 != 92_000_000 {
		t.Errorf("USD/EUR MidRateE8 = %d, want 92000000 (0.9200 scaled by 1e8)", eur.MidRateE8)
	}
	if eur.SpreadBps != 25 {
		t.Errorf("USD/EUR SpreadBps = %d, want 25", eur.SpreadBps)
	}

	bdt, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "USD", Quote: "BDT"})
	if err != nil {
		t.Fatalf("CurrentFXRate(USD, BDT) error = %v", err)
	}
	if bdt.MidRateE8 != 11_050_000_000 {
		t.Errorf("USD/BDT MidRateE8 = %d, want 11050000000 (110.50 scaled by 1e8)", bdt.MidRateE8)
	}
	if bdt.SpreadBps != 50 {
		t.Errorf("USD/BDT SpreadBps = %d, want 50", bdt.SpreadBps)
	}
}

// TestSeed_ParsesWholeNumberAndMinPrecision covers the two edges asked for
// in self-review: a whole number with no "." at all ("1" must scale to
// exactly domain.RateScale) and the smallest representable fraction
// ("0.00000001" must scale to exactly 1).
func TestSeed_ParsesWholeNumberAndMinPrecision(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	if err := fx.Seed(ctx, pool, "OOO:PPP=1/5,QQQ:RRR=0.00000001/1"); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	whole, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "OOO", Quote: "PPP"})
	if err != nil {
		t.Fatalf("CurrentFXRate(OOO, PPP) error = %v", err)
	}
	if whole.MidRateE8 != domain.RateScale {
		t.Errorf("OOO/PPP MidRateE8 = %d, want %d (a bare whole number is 1.0)", whole.MidRateE8, int64(domain.RateScale))
	}

	minimum, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "QQQ", Quote: "RRR"})
	if err != nil {
		t.Fatalf("CurrentFXRate(QQQ, RRR) error = %v", err)
	}
	if minimum.MidRateE8 != 1 {
		t.Errorf("QQQ/RRR MidRateE8 = %d, want 1 (the smallest representable rate)", minimum.MidRateE8)
	}
}

// TestSeed_EmptyIsNoop covers an unset or blank FX_RATES: not every
// deployment configures static rates, so Seed must do nothing rather than
// error.
func TestSeed_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()

	if err := fx.Seed(ctx, pool, "   "); err != nil {
		t.Fatalf("Seed(\"\") error = %v, want nil", err)
	}
}

// TestSeed_ReseedGuardsUnchangedButAppendsOnChange covers the re-seed
// behavior the task calls out explicitly: calling Seed again with the exact
// same entry must not duplicate the row (it would otherwise pile up an
// identical row every time the process restarts with the same FX_RATES),
// but calling it with a genuinely different rate for the same pair must
// still append, growing the pair's history the way a real rate change
// would, never overwriting the earlier row in place.
func TestSeed_ReseedGuardsUnchangedButAppendsOnChange(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	q := sqlc.New(pool)

	if err := fx.Seed(ctx, pool, "III:JJJ=1.50000000/10"); err != nil {
		t.Fatalf("Seed() first call error = %v", err)
	}
	if got := rowCount(t, "III", "JJJ"); got != 1 {
		t.Fatalf("row count after first seed = %d, want 1", got)
	}

	// Re-seed with the identical entry: guarded, no new row.
	if err := fx.Seed(ctx, pool, "III:JJJ=1.50000000/10"); err != nil {
		t.Fatalf("Seed() unchanged re-seed error = %v", err)
	}
	if got := rowCount(t, "III", "JJJ"); got != 1 {
		t.Errorf("row count after unchanged re-seed = %d, want still 1 (identical entry must not duplicate)", got)
	}

	// Re-seed with a changed rate for the same pair: this is a real rate
	// change, so it must append, not overwrite.
	if err := fx.Seed(ctx, pool, "III:JJJ=1.60000000/10"); err != nil {
		t.Fatalf("Seed() changed re-seed error = %v", err)
	}
	if got := rowCount(t, "III", "JJJ"); got != 2 {
		t.Errorf("row count after changed re-seed = %d, want 2 (a real rate change must append, growing history)", got)
	}

	current, err := q.CurrentFXRate(ctx, sqlc.CurrentFXRateParams{Base: "III", Quote: "JJJ"})
	if err != nil {
		t.Fatalf("CurrentFXRate(III, JJJ) error = %v", err)
	}
	if current.MidRateE8 != 160_000_000 {
		t.Errorf("current III/JJJ MidRateE8 = %d, want 160000000 (the latest seeded value)", current.MidRateE8)
	}
}

// TestSeed_RejectsMalformed covers the parsing failures the task requires
// Seed to reject rather than silently coerce or seed a bad row.
func TestSeed_RejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		wantErr error // non-nil to assert errors.Is; nil to only require a non-nil error
	}{
		{name: "missing colon in pair", raw: "USDEUR=0.92/25", wantErr: fx.ErrMalformedFXRate},
		{name: "missing equals", raw: "USD:EUR0.92/25", wantErr: fx.ErrMalformedFXRate},
		{name: "missing slash", raw: "USD:EUR=0.92", wantErr: fx.ErrMalformedFXRate},
		{name: "non numeric rate", raw: "USD:EUR=abc/25", wantErr: fx.ErrMalformedFXRate},
		{name: "non numeric spread", raw: "USD:EUR=0.92/abc", wantErr: fx.ErrMalformedFXRate},
		{name: "negative rate", raw: "USD:EUR=-0.92/25", wantErr: fx.ErrMalformedFXRate},
		{name: "lowercase currency", raw: "usd:EUR=0.92/25", wantErr: fx.ErrMalformedFXRate},
		{name: "wrong length currency code", raw: "US:EUR=0.92/25", wantErr: fx.ErrMalformedFXRate},
		{name: "base equals quote", raw: "USD:USD=0.92/25", wantErr: fx.ErrMalformedFXRate},
		{name: "zero rate", raw: "USD:EUR=0/25", wantErr: domain.ErrNonPositiveRate},
		{name: "spread at upper bound", raw: "USD:EUR=0.92/10000", wantErr: domain.ErrInvalidSpread},
		{name: "spread past upper bound", raw: "USD:EUR=0.92/99999", wantErr: domain.ErrInvalidSpread},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := newTestPool(t)
			ctx := context.Background()

			err := fx.Seed(ctx, pool, tc.raw)
			if err == nil {
				t.Fatalf("Seed(%q) error = nil, want an error", tc.raw)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Seed(%q) error = %v, want it to wrap %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}
