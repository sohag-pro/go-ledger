package domain

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
