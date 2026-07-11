package domain

import (
	"math"
	"testing"
)

func TestConvert_BankersRoundingSignSymmetric(t *testing.T) {
	usd, _ := NewMoney(1000, "USD") // 10.00 USD
	// mid 1.00000000, spread 0 -> exact
	got, applied, err := Convert(usd, "EUR", RateScale, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 1000 || got.Currency() != "EUR" {
		t.Fatalf("got %v", got)
	}
	if applied != RateScale {
		t.Fatalf("applied %d", applied)
	}
}

func TestConvert_HalfToEven(t *testing.T) {
	// source 5 minor, rate 0.5 -> 2.5 -> banker's -> 2
	src, _ := NewMoney(5, "USD")
	got, _, err := Convert(src, "EUR", RateScale/2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 2 {
		t.Fatalf("2.5 should round to 2 (even), got %d", got.Amount())
	}
	// source 7 minor, rate 0.5 -> 3.5 -> banker's -> 4
	src7, _ := NewMoney(7, "USD")
	got7, _, _ := Convert(src7, "EUR", RateScale/2, 0)
	if got7.Amount() != 4 {
		t.Fatalf("3.5 should round to 4 (even), got %d", got7.Amount())
	}
}

func TestConvert_NegativeSymmetric(t *testing.T) {
	src, _ := NewMoney(-5, "USD") // -2.5 -> -2
	got, _, _ := Convert(src, "EUR", RateScale/2, 0)
	if got.Amount() != -2 {
		t.Fatalf("-2.5 should round to -2, got %d", got.Amount())
	}
	src7, _ := NewMoney(-7, "USD") // -3.5 -> -4
	got7, _, _ := Convert(src7, "EUR", RateScale/2, 0)
	if got7.Amount() != -4 {
		t.Fatalf("-3.5 should round to -4, got %d", got7.Amount())
	}
}

func TestConvert_SpreadDisadvantagesCustomer(t *testing.T) {
	// mid 1.0, spread 100 bps (1%) -> applied 0.99 -> 10.00 USD -> 9.90 EUR (990 minor)
	src, _ := NewMoney(1000, "USD")
	got, applied, _ := Convert(src, "EUR", RateScale, 100)
	if got.Amount() != 990 {
		t.Fatalf("1%% spread should give 990, got %d", got.Amount())
	}
	if applied != 99_000_000 {
		t.Fatalf("applied 0.99, got %d", applied)
	}
}

func TestConvert_NoOverflow_LargeAmount(t *testing.T) {
	// source near int64 max, rate 2.0 -> would overflow int64 in a naive multiply
	src, _ := NewMoney(5_000_000_000_000_000, "USD") // 5e15
	got, _, err := Convert(src, "EUR", 2*RateScale, 0)
	if err != nil {
		t.Fatalf("must not overflow: %v", err)
	}
	if got.Amount() != 10_000_000_000_000_000 {
		t.Fatalf("got %d", got.Amount())
	}
}

func TestConvert_DustRejected(t *testing.T) {
	// source 1 minor, rate 0.4 -> 0.4 -> banker's -> 0 -> reject
	src, _ := NewMoney(1, "USD")
	_, _, err := Convert(src, "EUR", 40_000_000, 0)
	if err != ErrConversionDust {
		t.Fatalf("want ErrConversionDust, got %v", err)
	}
}

func TestConvert_NonPositiveRate(t *testing.T) {
	src, _ := NewMoney(100, "USD")
	if _, _, err := Convert(src, "EUR", 0, 0); err != ErrNonPositiveRate {
		t.Fatalf("want ErrNonPositiveRate, got %v", err)
	}
}

func TestConvert_InvalidSpread(t *testing.T) {
	src, _ := NewMoney(100, "USD")
	if _, _, err := Convert(src, "EUR", RateScale, 10_000); err != ErrInvalidSpread {
		t.Fatalf("spread of 100%% must be rejected, got %v", err)
	}
	if _, _, err := Convert(src, "EUR", RateScale, -1); err != ErrInvalidSpread {
		t.Fatalf("negative spread must be rejected, got %v", err)
	}
}

func TestConvert_MinInt64Source(t *testing.T) {
	// MinInt64 must not trip the uint64(-a) negation trap; big.Int handles it.
	src, _ := NewMoney(math.MinInt64, "USD")
	got, _, err := Convert(src, "EUR", RateScale, 0) // rate 1.0 -> identity
	if err != nil {
		t.Fatalf("MinInt64 at rate 1.0 must convert: %v", err)
	}
	if got.Amount() != math.MinInt64 {
		t.Fatalf("identity convert of MinInt64, got %d", got.Amount())
	}
}

func TestConvert_ResultOverflowRejected(t *testing.T) {
	// large source * large rate whose rounded result exceeds int64 -> ErrOverflow,
	// not a wrong wrapped value.
	src, _ := NewMoney(math.MaxInt64, "USD")
	if _, _, err := Convert(src, "EUR", 10*RateScale, 0); err != ErrOverflow {
		t.Fatalf("result past int64 must be ErrOverflow, got %v", err)
	}
}

// TestConvert_SpreadWithOrdinaryRounding verifies a mid+spread combination that
// is not a clean tie, so the result must come from ordinary (non-tie) rounding
// rather than the half-to-even branch. 100 minor at mid 1.0 with a 37 bps
// spread: factor = 1.0 * (10000-37)/10000 = 0.9963. 100 * 0.9963 = 99.63,
// which rounds normally to 100 (nearest even is irrelevant here, .63 rounds up).
func TestConvert_SpreadWithOrdinaryRounding(t *testing.T) {
	src, _ := NewMoney(100, "USD")
	got, applied, err := Convert(src, "EUR", RateScale, 37)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 100 {
		t.Fatalf("99.63 should round to 100, got %d", got.Amount())
	}
	if applied != 99_630_000 {
		t.Fatalf("applied should be 0.9963 scaled, got %d", applied)
	}
}

// TestConvert_NegativeSourceOrdinaryRounding verifies a negative source amount
// through a non-tie rounding case, confirming sign-symmetric rounding beyond
// the exact-half cases already covered above.
func TestConvert_NegativeSourceOrdinaryRounding(t *testing.T) {
	// -100 minor at rate 0.336 (RateScale*336/1000) -> -33.6 -> rounds to -34.
	src, _ := NewMoney(-100, "USD")
	got, _, err := Convert(src, "EUR", RateScale*336/1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != -34 {
		t.Fatalf("-33.6 should round to -34, got %d", got.Amount())
	}
}

// TestConvert_USDToJPY_ExponentFactor is the bug case from the audit: JPY has
// a zero-decimal minor unit, so converting 100.00 USD (10000 minor) at mid
// 150.0 (midE8 = 150e8) must land on 15000 yen, not 1,500,000 (the old code's
// answer, which is off by exactly the missing 10^(0-2) factor).
func TestConvert_USDToJPY_ExponentFactor(t *testing.T) {
	src, _ := NewMoney(10000, "USD") // 100.00 USD
	got, _, err := Convert(src, "JPY", 150*RateScale, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 15000 {
		t.Fatalf("100.00 USD at mid 150.0 should be 15000 JPY minor, got %d", got.Amount())
	}
	if got.Currency() != "JPY" {
		t.Fatalf("currency = %s, want JPY", got.Currency())
	}
}

// TestConvert_JPYToUSD_ExponentFactor exercises the reverse direction: JPY
// (0-dp) into USD (2-dp), so the exponent factor now widens the denominator
// instead of the numerator. midE8 = 666667 is round_half_even(1e8/150), the
// inverse of the 150.0 mid used above. 15000 yen at that mid works out to
// exactly 10000 USD minor units (100.00 USD): the remainder from the
// division is 5e9 against a denominator of 1e12, well under half, so it
// truncates rather than rounding up.
func TestConvert_JPYToUSD_ExponentFactor(t *testing.T) {
	src, _ := NewMoney(15000, "JPY") // 15000 yen
	got, _, err := Convert(src, "USD", 666667, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 10000 {
		t.Fatalf("15000 JPY at mid ~0.0066667 should be ~10000 USD minor (100.00 USD), got %d", got.Amount())
	}
	if got.Currency() != "USD" {
		t.Fatalf("currency = %s, want USD", got.Currency())
	}
}

// TestConvert_USDToBHD_ExponentFactor exercises a three-decimal currency
// (BHD): the exponent factor here is 10^1 (3-dp quote minus 2-dp base),
// applied to the numerator. 100.00 USD (10000 minor) at mid 0.377 BHD per USD
// (midE8 = 37,700,000) works out to exactly 37.700 BHD (37700 minor).
func TestConvert_USDToBHD_ExponentFactor(t *testing.T) {
	src, _ := NewMoney(10000, "USD") // 100.00 USD
	got, _, err := Convert(src, "BHD", 37_700_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount() != 37700 {
		t.Fatalf("100.00 USD at mid 0.377 should be 37700 BHD minor (37.700 BHD), got %d", got.Amount())
	}
	if got.Currency() != "BHD" {
		t.Fatalf("currency = %s, want BHD", got.Currency())
	}
}

// TestConvert_USDToEUR_Unchanged re-confirms that a same-exponent pair
// (both 2-dp) is unaffected by the exponent factor (diff == 0), guarding
// against a regression in the existing USD/EUR test coverage above.
func TestConvert_USDToEUR_Unchanged(t *testing.T) {
	src, _ := NewMoney(12345, "USD")                              // 123.45 USD
	got, applied, err := Convert(src, "EUR", 92*RateScale/100, 0) // mid 0.92
	if err != nil {
		t.Fatal(err)
	}
	// 123.45 * 0.92 = 113.574 -> banker's round -> 11357 minor (113.57 EUR).
	if got.Amount() != 11357 {
		t.Fatalf("123.45 USD at mid 0.92 should be 11357 EUR minor, got %d", got.Amount())
	}
	if applied != 92_000_000 {
		t.Fatalf("applied rate = %d, want 92000000", applied)
	}
}
