package domain

// MaxPostingDescriptionLen bounds a posting's free-text narration.
const MaxPostingDescriptionLen = 256

// Posting is one signed entry against a single account. The sign carries
// direction: a positive Amount is a debit, a negative Amount is a credit. This
// is the convention referenced throughout the domain (see ADR-002). A
// transaction's postings must sum to zero within each currency (see ADR-014).
// Description is an optional free-text narration for the line (for example
// "dinner repayment").
type Posting struct {
	AccountID   string
	Amount      Money
	Description string
}

// Validate checks that the posting names an account and that its description is
// within the length limit.
func (p Posting) Validate() error {
	if p.AccountID == "" {
		return ErrInvalidPosting
	}
	if len(p.Description) > MaxPostingDescriptionLen {
		return ErrDescriptionTooLong
	}
	return nil
}

// Transaction is an atomic, immutable set of postings that move money between
// accounts. Its defining invariant: for each currency present in the
// transaction, that currency's postings sum to zero (see ADR-014). A
// single-currency transaction is just the special case with one currency
// group; a cross-currency (FX) transaction routes each currency through its
// own clearing account so every currency group nets independently.
type Transaction struct {
	ID       string
	Postings []Posting
}

// Validate enforces the double-entry invariant. It requires at least two
// postings, every posting valid, and, for each currency present across the
// postings, that currency's signed amounts summing to exactly zero. It
// returns the first error found: ErrTooFewPostings, ErrInvalidPosting,
// ErrOverflow, or ErrUnbalanced.
//
// Postings are grouped by currency and accumulated with Money.Add, which is
// only ever called on two Money values of the same currency (every add is
// within one group), so ErrCurrencyMismatch can never fire here; a
// cross-currency transaction is not an error, it is the normal FX shape.
// Overflow detection stays centralized in Money.Add.
//
// Two things are deliberately allowed. A zero-amount posting is valid: a
// balanced set can legitimately include a zero leg (for example a zero fee), and
// the invariant is about the sum, not each leg. Repeated accounts are also
// allowed: a transaction may touch the same account in more than one posting (a
// reclassification can debit and credit the same account alongside other legs).
// Neither can break the balance invariant, so neither is rejected here.
func (t Transaction) Validate() error {
	if len(t.Postings) < 2 {
		return ErrTooFewPostings
	}
	for _, p := range t.Postings {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	sums := make(map[Currency]Money, len(t.Postings))
	for _, p := range t.Postings {
		cur := p.Amount.Currency()
		running, seen := sums[cur]
		if !seen {
			sums[cur] = p.Amount // first posting of this currency seeds the sum
			continue
		}
		next, err := running.Add(p.Amount) // same currency, so only ErrOverflow can fire
		if err != nil {
			return err
		}
		sums[cur] = next
	}
	for _, s := range sums {
		if !s.IsZero() {
			return ErrUnbalanced
		}
	}
	return nil
}
