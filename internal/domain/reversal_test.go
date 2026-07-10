package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestBuildReversal_NegatesEveryPostingAndLinks is the core contract: every
// posting comes back with the same account and currency but a negated
// amount, the new transaction carries newID, ReversesTransactionID points
// back at the original, and the result still validates (negation preserves
// the per-currency zero sum, ADR-014).
func TestBuildReversal_NegatesEveryPostingAndLinks(t *testing.T) {
	original := Transaction{
		ID: "txn-original",
		Postings: []Posting{
			{AccountID: "cash", Amount: mustMoney(t, 10000, "USD"), Description: "dinner"},
			{AccountID: "revenue", Amount: mustMoney(t, -10000, "USD"), Description: "dinner"},
		},
	}

	reversal, err := original.BuildReversal("txn-reversal")
	if err != nil {
		t.Fatalf("BuildReversal() error = %v", err)
	}
	if reversal.ID != "txn-reversal" {
		t.Errorf("ID = %q, want %q", reversal.ID, "txn-reversal")
	}
	if reversal.ReversesTransactionID == nil || *reversal.ReversesTransactionID != "txn-original" {
		t.Fatalf("ReversesTransactionID = %v, want pointer to %q", reversal.ReversesTransactionID, "txn-original")
	}
	if len(reversal.Postings) != len(original.Postings) {
		t.Fatalf("postings = %d, want %d", len(reversal.Postings), len(original.Postings))
	}
	for i, p := range reversal.Postings {
		orig := original.Postings[i]
		if p.AccountID != orig.AccountID {
			t.Errorf("posting %d account = %q, want %q", i, p.AccountID, orig.AccountID)
		}
		if p.Amount.Currency() != orig.Amount.Currency() {
			t.Errorf("posting %d currency = %s, want %s", i, p.Amount.Currency(), orig.Amount.Currency())
		}
		if p.Amount.Amount() != -orig.Amount.Amount() {
			t.Errorf("posting %d amount = %d, want %d", i, p.Amount.Amount(), -orig.Amount.Amount())
		}
		if !strings.Contains(p.Description, "txn-original") {
			t.Errorf("posting %d description = %q, want it to mention the original id", i, p.Description)
		}
	}
	if err := reversal.Validate(); err != nil {
		t.Errorf("reversal does not validate: %v", err)
	}
}

// TestBuildReversal_MultiCurrencyNegatesPerCurrency checks a convert-shaped,
// two-currency transaction: BuildReversal negates every leg regardless of
// currency, and each currency group still nets to zero afterward.
func TestBuildReversal_MultiCurrencyNegatesPerCurrency(t *testing.T) {
	original := Transaction{
		ID: "txn-convert",
		Postings: []Posting{
			{AccountID: "usd-account", Amount: mustMoney(t, -10000, "USD")},
			{AccountID: "usd-clearing", Amount: mustMoney(t, 10000, "USD")},
			{AccountID: "eur-clearing", Amount: mustMoney(t, -9200, "EUR")},
			{AccountID: "eur-account", Amount: mustMoney(t, 9200, "EUR")},
		},
	}

	reversal, err := original.BuildReversal("txn-convert-reversal")
	if err != nil {
		t.Fatalf("BuildReversal() error = %v", err)
	}
	if err := reversal.Validate(); err != nil {
		t.Fatalf("reversal does not validate: %v", err)
	}
	sums := map[Currency]int64{}
	for _, p := range reversal.Postings {
		sums[p.Amount.Currency()] += p.Amount.Amount()
	}
	if sums["USD"] != 0 {
		t.Errorf("USD postings sum = %d, want 0", sums["USD"])
	}
	if sums["EUR"] != 0 {
		t.Errorf("EUR postings sum = %d, want 0", sums["EUR"])
	}
}

// TestBuildReversal_EmptyNewIDLeavesIDEmpty documents the storage-assigns-id
// path: passing "" (what TransactionService.ReverseTransaction actually
// does, mirroring how Post and Convert leave a fresh transaction's ID empty
// for CreateTransaction to fill in) leaves the reversal's ID empty rather
// than inventing one here.
func TestBuildReversal_EmptyNewIDLeavesIDEmpty(t *testing.T) {
	original := Transaction{
		ID: "txn-original",
		Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
			{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
		},
	}
	reversal, err := original.BuildReversal("")
	if err != nil {
		t.Fatalf("BuildReversal() error = %v", err)
	}
	if reversal.ID != "" {
		t.Errorf("ID = %q, want empty", reversal.ID)
	}
}

// TestBuildReversal_OverflowPropagates checks that a posting whose amount is
// math.MinInt64 (the one value Money.Neg cannot negate) surfaces
// ErrOverflow rather than BuildReversal silently wrapping or panicking.
func TestBuildReversal_OverflowPropagates(t *testing.T) {
	original := Transaction{
		ID: "txn-original",
		Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, math.MinInt64, "USD")},
			{AccountID: "b", Amount: mustMoney(t, math.MaxInt64, "USD")},
		},
	}
	if _, err := original.BuildReversal("txn-reversal"); !errors.Is(err, ErrOverflow) {
		t.Errorf("BuildReversal() error = %v, want ErrOverflow", err)
	}
}

// TestBuildReversal_DescriptionRespectsLimit checks that even a pathologically
// long original id would not blow the posting description length limit: the
// prefix is truncated to MaxPostingDescriptionLen rather than left to fail
// Posting.Validate downstream.
func TestBuildReversal_DescriptionRespectsLimit(t *testing.T) {
	longID := strings.Repeat("x", MaxPostingDescriptionLen*2)
	original := Transaction{
		ID: longID,
		Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
			{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
		},
	}
	reversal, err := original.BuildReversal("txn-reversal")
	if err != nil {
		t.Fatalf("BuildReversal() error = %v", err)
	}
	for i, p := range reversal.Postings {
		if len(p.Description) > MaxPostingDescriptionLen {
			t.Errorf("posting %d description length = %d, want <= %d", i, len(p.Description), MaxPostingDescriptionLen)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("posting %d fails Validate: %v", i, err)
		}
	}
}
