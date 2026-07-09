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
