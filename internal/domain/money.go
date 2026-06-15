package domain

import "math"

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
	// Overflow occurred iff both operands share a sign and the result's sign
	// differs from theirs.
	if (m.amount > 0 && other.amount > 0 && sum < 0) ||
		(m.amount < 0 && other.amount < 0 && sum > 0) {
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
