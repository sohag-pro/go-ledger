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

func TestNewMoney(t *testing.T) {
	tests := []struct {
		name     string
		amount   int64
		currency Currency
		wantErr  error
	}{
		{"valid positive", 1050, "USD", nil},
		{"valid negative", -1050, "USD", nil},
		{"valid zero", 0, "USD", nil},
		{"invalid currency", 100, "usd", ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewMoney(tt.amount, tt.currency)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewMoney() err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				if m.Amount() != tt.amount {
					t.Errorf("Amount() = %d, want %d", m.Amount(), tt.amount)
				}
				if m.Currency() != tt.currency {
					t.Errorf("Currency() = %q, want %q", m.Currency(), tt.currency)
				}
			}
		})
	}
}

func TestMoneyIsZero(t *testing.T) {
	z, _ := NewMoney(0, "USD")
	if !z.IsZero() {
		t.Error("IsZero() = false for zero amount")
	}
	nz, _ := NewMoney(1, "USD")
	if nz.IsZero() {
		t.Error("IsZero() = true for non-zero amount")
	}
}
