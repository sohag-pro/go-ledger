package domain

import "testing"

func TestFingerprintStableAndDistinct(t *testing.T) {
	base := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	// Same content, rebuilt: identical fingerprint. ID is not part of it.
	same := Transaction{ID: "ignored", Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	if base.Fingerprint() != same.Fingerprint() {
		t.Error("same content produced different fingerprints")
	}
	if len(base.Fingerprint()) != 64 {
		t.Errorf("fingerprint length = %d, want 64 hex chars", len(base.Fingerprint()))
	}

	// Every semantic change must move the fingerprint.
	cases := map[string]Transaction{
		"amount": {Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 101, "USD")},
			{AccountID: "b", Amount: mustMoney(t, -101, "USD")},
		}},
		"account": {Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
			{AccountID: "c", Amount: mustMoney(t, -100, "USD")},
		}},
		"currency": {Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 100, "EUR")},
			{AccountID: "b", Amount: mustMoney(t, -100, "EUR")},
		}},
		"description": {Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "note"},
			{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
		}},
		"order": {Postings: []Posting{
			{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
			{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		}},
	}
	for name, tc := range cases {
		if tc.Fingerprint() == base.Fingerprint() {
			t.Errorf("%s: fingerprint did not change", name)
		}
	}
}
