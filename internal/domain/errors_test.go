package domain

import (
	"errors"
	"testing"
)

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{
		ErrCurrencyMismatch,
		ErrOverflow,
		ErrInvalidCurrency,
		ErrUnbalanced,
		ErrTooFewPostings,
		ErrInvalidAccountType,
	}
	for i := range all {
		for j := range all {
			if i != j && errors.Is(all[i], all[j]) {
				t.Errorf("errors at %d and %d compare equal; must be distinct", i, j)
			}
		}
	}
}
