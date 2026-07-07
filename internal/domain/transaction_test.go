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
