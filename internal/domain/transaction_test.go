package domain

import (
	"errors"
	"math"
	"testing"
)

func mustMoney(t *testing.T, amount int64, cur Currency) Money {
	t.Helper()
	m, err := NewMoney(amount, cur)
	if err != nil {
		t.Fatalf("NewMoney(%d, %q): %v", amount, cur, err)
	}
	return m
}

func TestPostingValidate(t *testing.T) {
	tests := []struct {
		name    string
		posting Posting
		wantErr error
	}{
		{
			name:    "valid",
			posting: Posting{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "dinner repayment"},
			wantErr: nil,
		},
		{
			name:    "empty account id",
			posting: Posting{AccountID: "", Amount: mustMoney(t, 100, "USD")},
			wantErr: ErrInvalidPosting,
		},
		{
			name: "description at limit",
			posting: Posting{
				AccountID:   "a",
				Amount:      mustMoney(t, 100, "USD"),
				Description: string(make([]byte, MaxPostingDescriptionLen)),
			},
			wantErr: nil,
		},
		{
			name: "description too long",
			posting: Posting{
				AccountID:   "a",
				Amount:      mustMoney(t, 100, "USD"),
				Description: string(make([]byte, MaxPostingDescriptionLen+1)),
			},
			wantErr: ErrDescriptionTooLong,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.posting.Validate(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestTransactionValidate(t *testing.T) {
	tests := []struct {
		name     string
		postings []Posting
		wantErr  error
	}{
		{
			name: "balanced two-leg",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
			},
			wantErr: nil,
		},
		{
			name: "balanced multi-leg",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -60, "USD")},
				{AccountID: "c", Amount: mustMoney(t, -40, "USD")},
			},
			wantErr: nil,
		},
		{
			name: "unbalanced",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -90, "USD")},
			},
			wantErr: ErrUnbalanced,
		},
		{
			name: "too few postings",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 0, "USD")},
			},
			wantErr: ErrTooFewPostings,
		},
		{
			name: "cross-currency, each currency balances on its own",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
				{AccountID: "c", Amount: mustMoney(t, 200, "EUR")},
				{AccountID: "d", Amount: mustMoney(t, -200, "EUR")},
			},
			wantErr: nil,
		},
		{
			name: "cross-currency, one currency unbalanced",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
				{AccountID: "c", Amount: mustMoney(t, 200, "EUR")},
				{AccountID: "d", Amount: mustMoney(t, -190, "EUR")},
			},
			wantErr: ErrUnbalanced,
		},
		{
			name: "empty account id",
			postings: []Posting{
				{AccountID: "", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
			},
			wantErr: ErrInvalidPosting,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := Transaction{ID: "tx_1", Postings: tt.postings}
			if err := tx.Validate(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_MultiCurrencyPerCurrencyZeroSum exercises the realistic FX
// shape from ADR-014: a user leg and a clearing leg per currency. Each
// currency group nets to zero independently, so the whole transaction
// validates even though no single running sum across all postings is zero
// in one currency.
func TestValidate_MultiCurrencyPerCurrencyZeroSum(t *testing.T) {
	usdA := mustMoney(t, -10000, "USD")
	usdB := mustMoney(t, 10000, "USD")
	eurA := mustMoney(t, -9200, "EUR")
	eurB := mustMoney(t, 9200, "EUR")
	tx := Transaction{Postings: []Posting{
		{AccountID: "u_usd", Amount: usdA},
		{AccountID: "fx_usd", Amount: usdB},
		{AccountID: "fx_eur", Amount: eurA},
		{AccountID: "u_eur", Amount: eurB},
	}}
	if err := tx.Validate(); err != nil {
		t.Fatalf("balanced per currency: %v", err)
	}
}

// TestValidate_PerCurrencyImbalanceRejected checks that a currency group
// that fails to net to zero is rejected even though other groups in the
// same transaction do balance.
func TestValidate_PerCurrencyImbalanceRejected(t *testing.T) {
	usdA := mustMoney(t, -10000, "USD")
	usdB := mustMoney(t, 10000, "USD")
	eurA := mustMoney(t, -9200, "EUR")
	eurB := mustMoney(t, 9100, "EUR") // EUR off by 100
	tx := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: usdA},
		{AccountID: "b", Amount: usdB},
		{AccountID: "c", Amount: eurA},
		{AccountID: "d", Amount: eurB},
	}}
	if err := tx.Validate(); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("EUR does not net to zero, want ErrUnbalanced, got %v", err)
	}
}

// TestValidate_PerCurrencyOverflow checks that an overflow in one currency's
// accumulation is still surfaced as ErrOverflow, even with other currencies
// present in the same transaction.
func TestValidate_PerCurrencyOverflow(t *testing.T) {
	tx := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, math.MaxInt64, "USD")},
		{AccountID: "b", Amount: mustMoney(t, math.MaxInt64, "USD")},
		{AccountID: "c", Amount: mustMoney(t, 100, "EUR")},
		{AccountID: "d", Amount: mustMoney(t, -100, "EUR")},
	}}
	if err := tx.Validate(); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}
