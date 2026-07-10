package domain

import (
	"fmt"
	"math"
)

// Currency is an ISO 4217 alphabetic code, for example "USD". v1 is
// single-currency, but Money carries its Currency so cross-currency arithmetic
// can be rejected structurally rather than silently producing wrong sums.
type Currency string

// Validate reports whether c is a well-formed currency code: exactly three
// uppercase ASCII letters. It does not check the code against the ISO 4217
// registry; shape validation is enough for v1.
func (c Currency) Validate() error {
	if len(c) != 3 {
		return ErrInvalidCurrency
	}
	for i := 0; i < len(c); i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return ErrInvalidCurrency
		}
	}
	return nil
}

// minorUnitsOverrides holds the currency codes whose minor-unit exponent is
// not the ISO 4217 default of 2 decimal places. Zero-decimal currencies (whole
// major units only) and three-decimal currencies are the two common
// exceptions; every code absent from this map defaults to 2 in MinorUnits.
var minorUnitsOverrides = map[Currency]int{
	// 0 decimals
	"JPY": 0,
	"KRW": 0,
	"VND": 0,
	"CLP": 0,
	"ISK": 0,
	"XOF": 0,
	"XAF": 0,
	"PYG": 0,
	"RWF": 0,
	"UGX": 0,
	// 3 decimals
	"BHD": 3,
	"KWD": 3,
	"OMR": 3,
	"TND": 3,
	"JOD": 3,
	"LYD": 3,
	"IQD": 3,
}

// MinorUnits returns the number of minor-unit decimal places for the currency
// (USD 2, JPY 0, BHD 3). Codes not in the registry default to 2, matching the
// ISO 4217 default and the ledger's pre-registry behavior; FX and formatting
// stay correct for the common set and degrade to the 2-dp default otherwise.
func (c Currency) MinorUnits() int {
	if n, ok := minorUnitsOverrides[c]; ok {
		return n
	}
	return 2
}

// Money is an immutable amount of a single currency, stored as a signed count
// of the currency's smallest unit (minor units; cents for USD). A positive
// amount is a debit, a negative amount a credit, by the convention documented
// on Posting. See ADR-002 for why int64 minor units.
type Money struct {
	amount   int64
	currency Currency
}

// NewMoney constructs a Money. It returns ErrInvalidCurrency if currency is not
// a well-formed code. amount may be any int64, including negative and zero.
func NewMoney(amount int64, currency Currency) (Money, error) {
	if err := currency.Validate(); err != nil {
		return Money{}, err
	}
	return Money{amount: amount, currency: currency}, nil
}

// Amount returns the signed minor-unit amount.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the currency code.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is zero. It ignores currency.
func (m Money) IsZero() bool { return m.amount == 0 }

// Add returns m + other. It returns ErrCurrencyMismatch if the currencies
// differ and ErrOverflow if the result exceeds int64 range.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, ErrCurrencyMismatch
	}
	sum := m.amount + other.amount
	// Signed overflow: adding a positive must not decrease the value, and
	// adding a negative must not increase it. This also catches
	// MinInt64 + MinInt64, which wraps all the way back to zero and would
	// otherwise be missed by a same-sign-flip check.
	if (other.amount > 0 && sum < m.amount) || (other.amount < 0 && sum > m.amount) {
		return Money{}, ErrOverflow
	}
	return Money{amount: sum, currency: m.currency}, nil
}

// Sub returns m - other. It returns ErrCurrencyMismatch if the currencies
// differ and ErrOverflow if the result exceeds int64 range.
func (m Money) Sub(other Money) (Money, error) {
	neg, err := other.Neg()
	if err != nil {
		return Money{}, err
	}
	return m.Add(neg)
}

// Neg returns -m. It returns ErrOverflow when the amount is math.MinInt64,
// whose negation is not representable as int64.
func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// String renders the amount with the currency's minor-unit decimal places
// (via Currency.MinorUnits) and the currency code, for example "10.50 USD"
// for a two-decimal currency, "15000 JPY" for a zero-decimal currency, or
// "10.500 BHD" for a three-decimal currency.
func (m Money) String() string {
	sign := ""
	a := uint64(m.amount) //nolint:gosec // intentional signed-to-unsigned reinterpretation for abs value
	if m.amount < 0 {
		sign = "-"
		a = -a // unsigned negation yields the correct magnitude, even for MinInt64
	}
	exp := m.currency.MinorUnits()
	if exp == 0 {
		return fmt.Sprintf("%s%d %s", sign, a, m.currency)
	}
	scale := uint64(1)
	for i := 0; i < exp; i++ {
		scale *= 10
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, a/scale, exp, a%scale, m.currency)
}
