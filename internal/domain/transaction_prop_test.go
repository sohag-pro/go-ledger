package domain

import (
	"errors"
	"testing"

	"pgregory.net/rapid"
)

// currencyPool is a fixed set of well-formed currency codes the multi-currency
// property tests below draw from. Drawing from a fixed pool (rather than
// generating random letters) keeps every draw a well-formed Currency without
// needing a filter, and rapid.Permutation gives distinct currencies per case
// for free.
var currencyPool = []Currency{"USD", "EUR", "GBP", "JPY", "CHF", "AUD", "CAD", "NZD", "SEK", "NOK"}

// genBalancedPostingsForCurrency builds a slice of postings, all in cur, that
// sum to exactly zero: n-1 random legs plus a final leg equal to the negation
// of their running sum. This is the same construction genBalancedPostings in
// invariant_test.go uses for a single currency, parameterized here so it can
// be called once per currency in a multi-currency transaction.
func genBalancedPostingsForCurrency(t *rapid.T, cur Currency) []Posting {
	n := rapid.IntRange(2, 6).Draw(t, "n")
	postings := make([]Posting, 0, n)
	var running int64
	for i := 0; i < n-1; i++ {
		amt := rapid.Int64Range(-1_000_000_000, 1_000_000_000).Draw(t, "amt")
		running += amt
		m, _ := NewMoney(amt, cur)
		postings = append(postings, Posting{AccountID: string(cur) + "-a", Amount: m})
	}
	last, _ := NewMoney(-running, cur)
	postings = append(postings, Posting{AccountID: string(cur) + "-z", Amount: last})
	return postings
}

// genMultiCurrencyTransaction draws K distinct currencies (K in [2,5]) from
// currencyPool, builds a per-currency-balanced posting group for each with
// genBalancedPostingsForCurrency, and shuffles all of them together into one
// flat slice, so the transaction's postings are not simply grouped by
// currency in generation order. Every currency group sums to zero on its own,
// so the whole transaction is expected to validate (Transaction.Validate
// groups by currency internally, so shuffling has no effect on correctness,
// only on making sure Validate really does regroup rather than relying on
// adjacency).
func genMultiCurrencyTransaction(t *rapid.T) []Posting {
	perm := rapid.Permutation(currencyPool).Draw(t, "currencyPerm")
	k := rapid.IntRange(2, 5).Draw(t, "k")
	var all []Posting
	for _, cur := range perm[:k] {
		all = append(all, genBalancedPostingsForCurrency(t, cur)...)
	}
	return rapid.Permutation(all).Draw(t, "shuffle")
}

// Property: a transaction built from K balanced per-currency posting groups
// (K currencies, each independently summing to zero, then shuffled together)
// always passes Validate. This is the multi-currency generalization of
// TestProp_BalancedAlwaysValid in invariant_test.go.
func TestProp_MultiCurrencyBalancedAlwaysValid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tx := Transaction{ID: "tx", Postings: genMultiCurrencyTransaction(t)}
		if err := tx.Validate(); err != nil {
			t.Fatalf("balanced multi-currency transaction rejected: %v", err)
		}
	})
}

// Property: perturbing exactly one leg of one currency group, in an otherwise
// balanced multi-currency transaction, by a non-zero minor-unit delta always
// makes Validate fail. The perturbation only ever touches one currency's
// running sum, so this is specifically proving the per-currency invariant
// catches a single-currency imbalance even when other currencies in the same
// transaction remain perfectly balanced around it.
func TestProp_MultiCurrencyPerturbedAlwaysInvalid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		postings := genMultiCurrencyTransaction(t)
		idx := rapid.IntRange(0, len(postings)-1).Draw(t, "idx")
		delta := rapid.Int64Range(1, 1_000_000).Draw(t, "delta")
		cur := postings[idx].Amount.Currency()
		bumped, _ := NewMoney(postings[idx].Amount.Amount()+delta, cur)
		postings[idx].Amount = bumped
		tx := Transaction{ID: "tx", Postings: postings}
		if err := tx.Validate(); err == nil {
			t.Fatalf("perturbed multi-currency transaction (currency %s off by %d) passed Validate, expected failure", cur, delta)
		}
	})
}

// genTwoCurrencyCancelingPerturbation builds a multi-currency transaction from
// K (K in [2,5]) per-currency-balanced groups, exactly like
// genMultiCurrencyTransaction, but then perturbs ONE posting in currency
// curs[0] by +delta and ONE posting in a DIFFERENT currency curs[1] by
// -delta. The two currencies are drawn without replacement from
// currencyPool, so they are always distinct.
//
// This is the case a currency-blind implementation cannot get right: because
// +delta and -delta are equal in raw minor units and opposite in sign, the
// naive sum of every posting's raw amount, ignoring currency entirely, is
// unchanged by the perturbation and stays exactly zero. A Validate that
// dumped every posting into one global accumulator (instead of grouping by
// currency) would therefore wrongly accept this transaction: real money
// would have vanished from curs[0] and reappeared, unaccounted for, in
// curs[1]. The real per-currency invariant must reject it, because curs[0]'s
// own group now sums to +delta and curs[1]'s own group now sums to -delta,
// each nonzero on its own.
func genTwoCurrencyCancelingPerturbation(t *rapid.T) ([]Posting, Currency, Currency, int64) {
	perm := rapid.Permutation(currencyPool).Draw(t, "currencyPerm")
	k := rapid.IntRange(2, 5).Draw(t, "k")
	curs := perm[:k]

	delta := rapid.Int64Range(1, 1_000_000).Draw(t, "delta")

	groups := make([][]Posting, k)
	for i, cur := range curs {
		groups[i] = genBalancedPostingsForCurrency(t, cur)
	}

	// Bump one leg of curs[0] by +delta and one leg of curs[1] by -delta:
	// raw-unit canceling, but each currency's own group is now off.
	idxA := rapid.IntRange(0, len(groups[0])-1).Draw(t, "idxA")
	idxB := rapid.IntRange(0, len(groups[1])-1).Draw(t, "idxB")
	bumpedA, _ := NewMoney(groups[0][idxA].Amount.Amount()+delta, curs[0])
	groups[0][idxA].Amount = bumpedA
	bumpedB, _ := NewMoney(groups[1][idxB].Amount.Amount()-delta, curs[1])
	groups[1][idxB].Amount = bumpedB

	var all []Posting
	for _, g := range groups {
		all = append(all, g...)
	}
	return rapid.Permutation(all).Draw(t, "shuffle"), curs[0], curs[1], delta
}

// Property: a transaction perturbed so that two DIFFERENT currencies are each
// individually unbalanced, by equal and opposite raw-unit deltas, must always
// be rejected with ErrUnbalanced. This is the case from the Task 10 review: a
// currency-blind global-sum check (sum every posting's amount regardless of
// currency; reject only if the grand total is nonzero) would see the two
// deltas cancel and wrongly accept the transaction, even though real money
// vanished from one currency and reappeared in another. Grouping by currency,
// as the real Validate does, is what makes this case fail as it must.
func TestProp_CrossCurrencyCancelingImbalanceAlwaysInvalid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		postings, curA, curB, delta := genTwoCurrencyCancelingPerturbation(t)
		tx := Transaction{ID: "tx", Postings: postings}
		err := tx.Validate()
		if !errors.Is(err, ErrUnbalanced) {
			t.Fatalf("cross-currency canceling imbalance (currency %s off by +%d, currency %s off by -%d) "+
				"gave err=%v, want ErrUnbalanced", curA, delta, curB, delta, err)
		}
	})
}
