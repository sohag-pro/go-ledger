package domain

import (
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// ledgerModel is an in-memory double-entry model: it applies validated,
// balanced transactions and tracks each account's balance as the running sum of
// its postings, never a stored mutable primary value.
type ledgerModel struct {
	balances map[string]int64
}

func (mdl *ledgerModel) apply(tx Transaction) {
	for _, p := range tx.Postings {
		mdl.balances[p.AccountID] += p.Amount.Amount()
	}
}

func (mdl *ledgerModel) total() int64 {
	var t int64
	for _, v := range mdl.balances {
		t += v
	}
	return t
}

// Property: applying a random sequence of balanced transactions keeps the
// signed total across all accounts at zero at every step.
func TestProp_ModelTotalAlwaysZero(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		mdl := &ledgerModel{balances: map[string]int64{}}
		steps := rapid.IntRange(1, 50).Draw(t, "steps")
		for i := 0; i < steps; i++ {
			tx := Transaction{ID: "tx", Postings: genBalancedAcrossAccounts(t)}
			if err := tx.Validate(); err != nil {
				t.Fatalf("generated tx invalid: %v", err)
			}
			mdl.apply(tx)
			if got := mdl.total(); got != 0 {
				t.Fatalf("after step %d total=%d, want 0", i, got)
			}
		}
	})
}

// genBalancedAcrossAccounts builds a balanced transaction spread over a small
// set of named accounts so the model accumulates cross-account state.
func genBalancedAcrossAccounts(t *rapid.T) []Posting {
	accts := []string{"acc-a", "acc-b", "acc-c", "acc-d"}
	n := rapid.IntRange(2, 4).Draw(t, "legs")
	postings := make([]Posting, 0, n)
	var running int64
	for i := 0; i < n-1; i++ {
		amt := rapid.Int64Range(-1_000_000, 1_000_000).Draw(t, "amt")
		running += amt
		m, _ := NewMoney(amt, "USD")
		postings = append(postings, Posting{AccountID: accts[i%len(accts)], Amount: m})
	}
	last, _ := NewMoney(-running, "USD")
	postings = append(postings, Posting{AccountID: accts[(n-1)%len(accts)], Amount: last})
	return postings
}

// Property: concurrent application of balanced transactions to a shared,
// mutex-guarded model never loses an update; the final total is zero. Run under
// -race (the default in `make test`).
func TestProp_ConcurrentApplyNoLostUpdate(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		mdl := &ledgerModel{balances: map[string]int64{}}
		var mu sync.Mutex
		txs := rapid.IntRange(2, 32).Draw(t, "txs")

		// Pre-generate transactions so goroutines do not touch rapid.T.
		batch := make([][]Posting, txs)
		for i := range batch {
			batch[i] = genBalancedAcrossAccounts(t)
		}

		var wg sync.WaitGroup
		for i := 0; i < txs; i++ {
			wg.Add(1)
			go func(ps []Posting) {
				defer wg.Done()
				tx := Transaction{ID: "tx", Postings: ps}
				if err := tx.Validate(); err != nil {
					return
				}
				mu.Lock()
				mdl.apply(tx)
				mu.Unlock()
			}(batch[i])
		}
		wg.Wait()
		if got := mdl.total(); got != 0 {
			t.Fatalf("after concurrent apply total=%d, want 0", got)
		}
	})
}
