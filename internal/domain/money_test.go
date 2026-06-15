package domain

import (
	"errors"
	"testing"
)

func TestCurrencyValidate(t *testing.T) {
	tests := []struct {
		name    string
		code    Currency
		wantErr error
	}{
		{"valid USD", "USD", nil},
		{"valid EUR", "EUR", nil},
		{"lowercase rejected", "usd", ErrInvalidCurrency},
		{"too short", "US", ErrInvalidCurrency},
		{"too long", "USDX", ErrInvalidCurrency},
		{"empty", "", ErrInvalidCurrency},
		{"digits rejected", "US1", ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.code.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
