package domain

// MaxPostingDescriptionLen bounds a posting's free-text narration.
const MaxPostingDescriptionLen = 256

// Posting is one signed entry against a single account. The sign carries
// direction: a positive Amount is a debit, a negative Amount is a credit. This
// is the convention referenced throughout the domain (see ADR-002). A
// transaction's postings must sum to zero. Description is an optional free-text
// narration for the line (for example "dinner repayment").
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
// accounts. Its defining invariant: the postings sum to zero in a single
// currency. Validate is the one place that invariant is enforced in the domain
// (the database adds a CHECK constraint in Week 4).
type Transaction struct {
	ID       string
	Postings []Posting
}

// Validate enforces the double-entry invariant. It requires at least two
// postings, every posting valid, all postings in the same currency, and the
// signed amounts summing to exactly zero. It returns the first error found:
// ErrTooFewPostings, ErrInvalidPosting, ErrCurrencyMismatch, ErrOverflow, or
// ErrUnbalanced.
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
	sum := t.Postings[0].Amount
	for _, p := range t.Postings[1:] {
		next, err := sum.Add(p.Amount) // surfaces ErrCurrencyMismatch and ErrOverflow
		if err != nil {
			return err
		}
		sum = next
	}
	if !sum.IsZero() {
		return ErrUnbalanced
	}
	return nil
}
