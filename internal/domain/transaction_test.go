package domain

import (
	"errors"
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
			name: "currency mismatch",
			postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
				{AccountID: "b", Amount: mustMoney(t, -100, "EUR")},
			},
			wantErr: ErrCurrencyMismatch,
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
