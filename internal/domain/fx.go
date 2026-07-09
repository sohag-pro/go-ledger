package domain

import "math/big"

// RateScale is the fixed-point scale for FX rates: a rate is an integer count of
// hundred-millionths of a quote unit per base unit. 1.0 == RateScale.
const RateScale = 100_000_000 // 1e8

// bpsScale is the basis-point denominator for the spread (10000 bps == 100%).
const bpsScale = 10_000

// FXQuote is a mid rate, quote units per one base unit, scaled by RateScale.
type FXQuote struct {
	Base, Quote Currency
	MidRateE8   int64
}

var (
	bigRateScale = big.NewInt(RateScale)
	bigBpsScale  = big.NewInt(bpsScale)
	bigOne       = big.NewInt(1)
)

// Convert turns source (in the base currency) into the quote currency at the mid
// rate midE8 (quote per base, scaled 1e8) after widening by spreadBps against the
// customer. It returns the converted Money, the applied rate (scaled 1e8, an
// informational snapshot), and an error. Integer only: no float64 ever touches
// money, and the whole product is computed in big.Int so it cannot overflow.
//
// The customer always receives fewer quote units than the mid implies:
//
//	converted = bankersRound( source * midE8 * (bpsScale - spreadBps)
//	                          / (RateScale * bpsScale) )
//
// This is a SINGLE rounding step (not rate-round then amount-round), so there is
// no double-rounding bias. The reproducible truth of a conversion is
// (midE8, spreadBps, source) run through this formula; appliedE8 is a convenience
// rounding of midE8 * (bpsScale - spreadBps) / bpsScale for display.
func Convert(source Money, quote Currency, midE8 int64, spreadBps int32) (Money, int64, error) {
	if midE8 <= 0 {
		return Money{}, 0, ErrNonPositiveRate
	}
	if spreadBps < 0 || spreadBps >= bpsScale {
		return Money{}, 0, ErrInvalidSpread
	}
	// factor = midE8 * (bpsScale - spreadBps); kept in big.Int (can exceed int64).
	factor := new(big.Int).Mul(big.NewInt(midE8), big.NewInt(int64(bpsScale-spreadBps)))
	if factor.Sign() <= 0 {
		return Money{}, 0, ErrNonPositiveRate
	}
	// converted = round_half_even( source * factor / (RateScale * bpsScale) )
	num := new(big.Int).Mul(big.NewInt(source.amount), factor)
	den := new(big.Int).Mul(bigRateScale, bigBpsScale) // 1e8 * 1e4 = 1e12
	converted, ok := bankersDiv(num, den)
	if !ok {
		return Money{}, 0, ErrOverflow // result does not fit int64 minor units
	}
	if converted == 0 && source.amount != 0 {
		return Money{}, 0, ErrConversionDust
	}
	// appliedE8 (informational): round_half_even(factor / bpsScale), scaled 1e8.
	appliedE8, _ := bankersDiv(factor, bigBpsScale)
	out, err := NewMoney(converted, quote)
	if err != nil {
		return Money{}, 0, err
	}
	return out, appliedE8, nil
}

// bankersDiv returns round_half_to_even(num / den) as int64, sign-symmetric.
// den must be positive. ok is false if the rounded result does not fit int64.
func bankersDiv(num, den *big.Int) (int64, bool) {
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(num, den, r) // q truncated toward zero; r carries the sign of num
	r.Abs(r)
	twice := new(big.Int).Lsh(r, 1) // 2 * |r|
	switch cmp := twice.Cmp(den); {
	case cmp > 0, cmp == 0 && q.Bit(0) == 1: // past half, or exactly half and q is odd
		if num.Sign() < 0 {
			q.Sub(q, bigOne) // away from zero (more negative)
		} else {
			q.Add(q, bigOne) // away from zero (more positive)
		}
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}
