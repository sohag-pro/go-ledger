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

// TestFingerprintDiffersByPostingCurrency checks that two transactions
// differing only in one posting's currency (with the rest of the shape
// identical, including that each still balances on its own) get different
// fingerprints. This is the per-posting-currency behavior introduced by
// ADR-014 decision 9: there is no longer a single transaction-level currency
// to hash, so the currency has to move into the per-posting fields, and a
// change there must still move the fingerprint.
func TestFingerprintDiffersByPostingCurrency(t *testing.T) {
	// The first posting's currency is USD in both cases: with the old
	// transaction-level-currency scheme, only postings[0]'s currency was
	// hashed, so a change confined to a later posting's currency would have
	// been invisible to the fingerprint. That is exactly the gap this test
	// guards against.
	usd := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	eur := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "EUR")},
	}}
	if usd.Fingerprint() == eur.Fingerprint() {
		t.Error("changing one posting's currency did not change the fingerprint")
	}
}

// TestFingerprintDiffersByPostingDescription checks that two transactions
// differing only in one posting's description get different fingerprints.
// Description is deliberately kept in the per-posting hash (ADR-014 decision
// 9): dropping it would let a reused idempotency key with a different
// description silently replay the wrong stored transaction instead of
// returning 409.
func TestFingerprintDiffersByPostingDescription(t *testing.T) {
	a := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "rent"},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	b := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "groceries"},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("changing one posting's description did not change the fingerprint")
	}
}

// TestFingerprintNoBoundaryCollision proves the length-prefixed framing keeps
// adversarial pairs distinct even though a plain separator scheme would let
// bytes straddle a field boundary and collide.
func TestFingerprintNoBoundaryCollision(t *testing.T) {
	cases := []struct {
		name string
		a    Transaction
		b    Transaction
	}{
		{
			name: "byte moved across account/description boundary",
			a: Transaction{Postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "b"},
				{AccountID: "x", Amount: mustMoney(t, -100, "USD")},
			}},
			b: Transaction{Postings: []Posting{
				{AccountID: "ab", Amount: mustMoney(t, 100, "USD"), Description: ""},
				{AccountID: "x", Amount: mustMoney(t, -100, "USD")},
			}},
		},
		{
			name: "embedded NUL in description vs none",
			a: Transaction{Postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "a\x00b"},
				{AccountID: "x", Amount: mustMoney(t, -100, "USD")},
			}},
			b: Transaction{Postings: []Posting{
				{AccountID: "a", Amount: mustMoney(t, 100, "USD"), Description: "ab"},
				{AccountID: "x", Amount: mustMoney(t, -100, "USD")},
			}},
		},
	}
	for _, tc := range cases {
		if tc.a.Fingerprint() == tc.b.Fingerprint() {
			t.Errorf("%s: adversarial pair collided", tc.name)
		}
	}
}
