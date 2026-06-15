package domain

import (
	"testing"

	"pgregory.net/rapid"
)

// genBalancedPostings builds a slice of postings that is guaranteed to balance:
// it generates n-1 random legs, then adds a final leg equal to the negation of
// their running sum. Amounts are bounded well within int64 to avoid overflow,
// which is exercised separately by the unit tests.
func genBalancedPostings(t *rapid.T) []Posting {
	n := rapid.IntRange(2, 8).Draw(t, "n")
	postings := make([]Posting, 0, n)
	var running int64
	for i := 0; i < n-1; i++ {
		amt := rapid.Int64Range(-1_000_000_000, 1_000_000_000).Draw(t, "amt")
		running += amt
		m, _ := NewMoney(amt, "USD")
		postings = append(postings, Posting{AccountID: "a", Amount: m})
	}
	last, _ := NewMoney(-running, "USD")
	postings = append(postings, Posting{AccountID: "z", Amount: last})
	return postings
}

// Property: any balanced set of postings passes Validate.
func TestProp_BalancedAlwaysValid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tx := Transaction{ID: "tx", Postings: genBalancedPostings(t)}
		if err := tx.Validate(); err != nil {
			t.Fatalf("balanced transaction rejected: %v", err)
		}
	})
}

// Property: perturbing exactly one leg of a balanced transaction by a non-zero
// delta always makes it unbalanced.
func TestProp_PerturbedAlwaysInvalid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		postings := genBalancedPostings(t)
		idx := rapid.IntRange(0, len(postings)-1).Draw(t, "idx")
		delta := rapid.Int64Range(1, 1_000_000).Draw(t, "delta")
		bumped, _ := NewMoney(postings[idx].Amount.Amount()+delta, "USD")
		postings[idx].Amount = bumped
		tx := Transaction{ID: "tx", Postings: postings}
		if err := tx.Validate(); err == nil {
			t.Fatal("perturbed transaction passed Validate, expected failure")
		}
	})
}

// Property: a single-currency balanced transaction's amounts sum to zero by
// construction; verify the running sum we compute matches.
func TestProp_SumIsZero(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		postings := genBalancedPostings(t)
		var sum int64
		for _, p := range postings {
			sum += p.Amount.Amount()
		}
		if sum != 0 {
			t.Fatalf("generator produced non-zero sum %d", sum)
		}
	})
}
