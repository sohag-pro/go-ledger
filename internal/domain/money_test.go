package domain

import (
	"errors"
	"math"
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

func TestMoneyAdd(t *testing.T) {
	tests := []struct {
		name    string
		a, b    int64
		ca, cb  Currency
		want    int64
		wantErr error
	}{
		{"simple", 100, 50, "USD", "USD", 150, nil},
		{"with negative", 100, -150, "USD", "USD", -50, nil},
		{"currency mismatch", 100, 50, "USD", "EUR", 0, ErrCurrencyMismatch},
		{"overflow positive", math.MaxInt64, 1, "USD", "USD", 0, ErrOverflow},
		{"overflow negative", math.MinInt64, -1, "USD", "USD", 0, ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := NewMoney(tt.a, tt.ca)
			b, _ := NewMoney(tt.b, tt.cb)
			got, err := a.Add(b)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Add() err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got.Amount() != tt.want {
				t.Errorf("Add() = %d, want %d", got.Amount(), tt.want)
			}
		})
	}
}

func TestMoneySub(t *testing.T) {
	a, _ := NewMoney(100, "USD")
	b, _ := NewMoney(30, "USD")
	got, err := a.Sub(b)
	if err != nil || got.Amount() != 70 {
		t.Fatalf("Sub() = %d, %v; want 70, nil", got.Amount(), err)
	}
	eur, _ := NewMoney(1, "EUR")
	if _, err := a.Sub(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub() cross-currency err = %v, want ErrCurrencyMismatch", err)
	}
	mn, _ := NewMoney(math.MinInt64, "USD")
	one, _ := NewMoney(1, "USD")
	if _, err := mn.Sub(one); !errors.Is(err, ErrOverflow) {
		t.Errorf("Sub() underflow err = %v, want ErrOverflow", err)
	}
}

func TestMoneyNeg(t *testing.T) {
	a, _ := NewMoney(100, "USD")
	if n, _ := a.Neg(); n.Amount() != -100 {
		t.Errorf("Neg() = %d, want -100", n.Amount())
	}
	mn, _ := NewMoney(math.MinInt64, "USD")
	if _, err := mn.Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Neg() of MinInt64 err = %v, want ErrOverflow", err)
	}
}

func TestMoneyString(t *testing.T) {
	tests := []struct {
		amount int64
		want   string
	}{
		{1050, "10.50 USD"},
		{-1050, "-10.50 USD"},
		{5, "0.05 USD"},
		{-5, "-0.05 USD"},
		{0, "0.00 USD"},
		{100, "1.00 USD"},
		{math.MinInt64, "-92233720368547758.08 USD"},
	}
	for _, tt := range tests {
		m, _ := NewMoney(tt.amount, "USD")
		if got := m.String(); got != tt.want {
			t.Errorf("String() for %d = %q, want %q", tt.amount, got, tt.want)
		}
	}
}
