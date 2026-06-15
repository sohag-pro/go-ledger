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
