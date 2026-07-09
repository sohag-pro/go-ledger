package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestConvertRoundTrip_USDToEURToUSD_ReconcilesToClearing is the money-safety
// proof for FX (Task 10, ADR-014): converting X USD into EUR and immediately
// back into USD must never make money vanish. Two spreads and a rounding
// residual do cost the user something on the round trip; this test proves
// that cost is fully accounted for, not leaked: it decomposes cleanly into
// the two spreads plus a rounding residual, and the same amount lands, to
// the minor unit, in the USD FX clearing account. Every currency in the
// book, clearing accounts included, still nets to zero afterward.
func TestConvertRoundTrip_USDToEURToUSD_ReconcilesToClearing(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	svc := newConvertService(pool)
	ctx := context.Background()
	tenant := uuid.NewString()

	const (
		base, quote = domain.Currency("USD"), domain.Currency("EUR")
		midE8       = 92_000_000 // 0.92 EUR per USD
		spreadBps   = 75         // 0.75%, large enough that spread cost cannot be confused with rounding jitter
	)
	seedConvertRate(t, pool, quote, midE8, spreadBps)

	usd := newConvertAccount(t, repo, tenant, base)
	eur := newConvertAccount(t, repo, tenant, quote)

	const sourceAmount = 1_000_000 // $10,000.00: large enough that neither leg dusts

	// Leg 1: USD -> EUR.
	fwdReq := ledger.ConvertRequest{FromAccountID: usd.ID, ToAccountID: eur.ID, SourceAmount: sourceAmount}
	fwd, replayed, err := svc.Convert(ctx, tenant, fwdReq, &domain.Idempotency{Key: "roundtrip-fwd"})
	if err != nil {
		t.Fatalf("forward Convert() error = %v", err)
	}
	if replayed {
		t.Fatalf("forward Convert() replayed = true, want false")
	}
	if fwd.FX == nil {
		t.Fatalf("forward Convert() has no FX detail")
	}
	amountEUR := fwd.FX.ConvertedAmount
	mid1, spread1bps := fwd.FX.MidRateE8, fwd.FX.SpreadBps

	// Leg 2: EUR -> USD, converting the ENTIRE EUR amount just credited, so
	// the EUR clearing account carries no leftover open position afterward.
	backReq := ledger.ConvertRequest{FromAccountID: eur.ID, ToAccountID: usd.ID, SourceAmount: amountEUR}
	back, replayed, err := svc.Convert(ctx, tenant, backReq, &domain.Idempotency{Key: "roundtrip-back"})
	if err != nil {
		t.Fatalf("return Convert() error = %v", err)
	}
	if replayed {
		t.Fatalf("return Convert() replayed = true, want false")
	}
	if back.FX == nil {
		t.Fatalf("return Convert() has no FX detail")
	}
	finalUSD := back.FX.ConvertedAmount
	mid2, spread2bps := back.FX.MidRateE8, back.FX.SpreadBps

	// --- (a) decompose the round trip's cost into named, individually checked pieces ---
	//
	// Recompute both legs with domain.Convert (the same pure function the
	// service itself calls) at the ACTUAL rates the service resolved, but
	// with the spread stripped to zero. That isolates what a pure mid-rate
	// round trip would have cost (rounding only, no markup) from what each
	// leg's own spread additionally cost. residual, spread1USD, and spread2USD
	// are defined below by telescoping subtraction, so residual + spread1USD +
	// spread2USD == totalLoss by construction: that arithmetic identity is not
	// itself a finding, so it is not asserted. What is checked, and does prove
	// something, is that each named piece is well-behaved on its own: both
	// spreads are non-negative and the rounding-only residual stays within its
	// expected one-unit-per-leg bound. Part (b) below is the real conservation
	// proof: the total loss lands, to the minor unit, in the USD clearing
	// account.
	sourceMoney, err := domain.NewMoney(sourceAmount, base)
	if err != nil {
		t.Fatalf("NewMoney(source): %v", err)
	}
	idealForward, _, err := domain.Convert(sourceMoney, quote, mid1, 0)
	if err != nil {
		t.Fatalf("idealForward: %v", err)
	}
	actualForward, _, err := domain.Convert(sourceMoney, quote, mid1, spread1bps)
	if err != nil {
		t.Fatalf("actualForward: %v", err)
	}
	if actualForward.Amount() != amountEUR {
		t.Fatalf("actualForward = %d, want %d (the service's own converted amount)", actualForward.Amount(), amountEUR)
	}

	idealReturn, _, err := domain.Convert(idealForward, base, mid2, 0)
	if err != nil {
		t.Fatalf("idealReturn: %v", err)
	}
	idealReturn2, _, err := domain.Convert(actualForward, base, mid2, 0)
	if err != nil {
		t.Fatalf("idealReturn2: %v", err)
	}
	actualReturn, _, err := domain.Convert(actualForward, base, mid2, spread2bps)
	if err != nil {
		t.Fatalf("actualReturn: %v", err)
	}
	if actualReturn.Amount() != finalUSD {
		t.Fatalf("actualReturn = %d, want %d (the service's own converted amount)", actualReturn.Amount(), finalUSD)
	}

	// residual: what a pure mid-rate round trip (zero spread on both legs)
	// would still have cost, in USD, purely from minor-unit rounding.
	residual := sourceAmount - idealReturn.Amount()
	// spread1USD: leg 1's own spread cost, expressed in USD by running the
	// SAME leg-2 mid rate over both the zero-spread and the actual leg-1
	// outputs.
	spread1USD := idealReturn.Amount() - idealReturn2.Amount()
	// spread2USD: leg 2's own spread cost, already in USD.
	spread2USD := idealReturn2.Amount() - actualReturn.Amount()
	totalLoss := sourceAmount - finalUSD

	// domain.Convert never rounds in the customer's favor once a spread is
	// applied (see its doc comment), so each leg's own spread cost must be
	// non-negative.
	if spread1USD < 0 {
		t.Errorf("spread1USD = %d, want >= 0 (spread never favors the customer)", spread1USD)
	}
	if spread2USD < 0 {
		t.Errorf("spread2USD = %d, want >= 0 (spread never favors the customer)", spread2USD)
	}
	// The rounding-only residual is bounded: each domain.Convert call rounds
	// half-to-even to the nearest minor unit, so a two-leg pure-mid round
	// trip can drift by at most one unit per leg.
	if residual < -2 || residual > 2 {
		t.Errorf("residual = %d, want within [-2, 2] (rounding-only drift across two conversions)", residual)
	}
	if totalLoss <= 0 {
		t.Fatalf("totalLoss = %d, want > 0 (a real spread on both legs must cost something)", totalLoss)
	}

	// --- (b) the total lost equals the net position sitting in the FX clearing accounts ---
	clearingUSD, err := repo.GetOrCreateClearingAccount(ctx, tenant, base)
	if err != nil {
		t.Fatalf("get clearing USD: %v", err)
	}
	clearingEUR, err := repo.GetOrCreateClearingAccount(ctx, tenant, quote)
	if err != nil {
		t.Fatalf("get clearing EUR: %v", err)
	}
	clearingUSDBal, err := repo.Balance(ctx, tenant, clearingUSD.ID)
	if err != nil {
		t.Fatalf("balance clearing USD: %v", err)
	}
	clearingEURBal, err := repo.Balance(ctx, tenant, clearingEUR.ID)
	if err != nil {
		t.Fatalf("balance clearing EUR: %v", err)
	}
	if clearingUSDBal.Amount() != totalLoss {
		t.Errorf("clearing USD balance = %d, want totalLoss %d (nothing vanishes, it all sits in clearing)",
			clearingUSDBal.Amount(), totalLoss)
	}
	// Converting the ENTIRE EUR credit back means leg 2's source amount
	// exactly matches leg 1's converted amount, so leg 1's open EUR position
	// is fully closed out by leg 2: no EUR is left stranded in clearing.
	if !clearingEURBal.IsZero() {
		t.Errorf("clearing EUR balance = %s, want zero (round trip closes the EUR clearing position)", clearingEURBal)
	}

	// --- (c) every currency still nets to zero across the whole book ---
	accounts, err := repo.ListAccounts(ctx, tenant, 100)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 4 {
		t.Fatalf("tenant accounts = %d, want 4 (usd, eur, and their two clearing accounts)", len(accounts))
	}
	sums := map[domain.Currency]int64{}
	for _, a := range accounts {
		bal, err := repo.Balance(ctx, tenant, a.ID)
		if err != nil {
			t.Fatalf("balance %s: %v", a.ID, err)
		}
		sums[a.Currency] += bal.Amount()
	}
	for cur, sum := range sums {
		if sum != 0 {
			t.Errorf("book-wide %s sum = %d, want 0 (per-currency invariant across the whole book)", cur, sum)
		}
	}
}
