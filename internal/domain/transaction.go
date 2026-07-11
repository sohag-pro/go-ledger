package domain

import "time"

// MaxPostingDescriptionLen bounds a posting's free-text narration.
const MaxPostingDescriptionLen = 256

// MaxTransactionReferenceLen bounds a transaction's optional external
// reference (Task 4.3, audit A1.3), reusing the same 256 cap as a posting's
// description: there is nothing special about that number here, it is just a
// sane, already-established ceiling for a free-form identifier a client
// supplies.
const MaxTransactionReferenceLen = 256

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
	// ID is the posting's own persisted id. Empty on a Posting a caller
	// builds for CreateTransaction (the adapter assigns one at insert time,
	// mirroring Transaction.ID); populated on a Posting read back from
	// storage, for example by GetTransaction or ListTransactions (Task 4.4,
	// audit A7.2), which needs a posting's own id for its flattened,
	// posting-level CSV export.
	ID string
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
	// FX is the immutable snapshot of the conversion applied, when this
	// transaction is a cross-currency convert (ADR-014 decision 7). It is nil
	// for an ordinary transaction: Fingerprint deliberately does not hash FX,
	// since it is not part of the double-entry content Fingerprint protects
	// (a convert's idempotency fingerprint is computed over the request, not
	// the postings; see internal/ledger's Convert).
	FX *FXDetail
	// ReversesTransactionID is the id of the transaction this one reverses
	// (Task 4.2, audit A1.2), or nil for an ordinary post. Postings are
	// append-only (ADR-001): a reversal is never a mutation of the original,
	// it is a brand new transaction, built by BuildReversal, whose postings
	// undo it. A transaction with this set is itself never reversible again
	// (see ErrCannotReverseReversal): there is no reversal of a reversal in
	// this model, only forward corrections.
	ReversesTransactionID *string
	// Reference is an optional, client-supplied external id for
	// reconciliation against an upstream system (Task 4.3, audit A1.3): a
	// bank statement line, a payment processor's charge id, and so on. Unique
	// per tenant when present (migration 0018's transactions_tenant_reference_idx),
	// nil when the caller supplies none. A duplicate within the same tenant
	// surfaces as ErrDuplicateReference from CreateTransaction, distinct from
	// the idempotency-key conflict: the same request retried with the same
	// Idempotency-Key replays; a different request that happens to reuse
	// someone else's reference is rejected instead.
	Reference *string
	// EffectiveAt is the value date: when the transaction is considered to
	// have happened economically, as distinct from when the row was actually
	// written (Task 4.3, audit A1.3). Optional on input (nil means the caller
	// supplied none); on read from storage it is never nil, since the
	// adapter falls back to the row's created_at rather than leaving it
	// unset (see postgres.Repository.transactionFromRow).
	EffectiveAt *time.Time
}

// Validate enforces the double-entry invariant. It requires at least two
// postings, every posting valid, and, for each currency present across the
// postings, that currency's signed amounts summing to exactly zero. It
// returns the first error found: ErrTooFewPostings, ErrInvalidPosting,
// ErrOverflow, ErrInvalidReference, ErrReferenceTooLong, or ErrUnbalanced.
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
	if t.Reference != nil {
		if *t.Reference == "" {
			return ErrInvalidReference
		}
		if len(*t.Reference) > MaxTransactionReferenceLen {
			return ErrReferenceTooLong
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

// reversalDescriptionPrefix narrates a reversal posting: "reversal of
// <original transaction id>". It is truncated to MaxPostingDescriptionLen
// if ever combined with an id long enough to exceed it (never true for a
// UUID today, but BuildReversal enforces the limit rather than assume it).
const reversalDescriptionPrefix = "reversal of "

// BuildReversal returns the transaction that reverses t: a new transaction
// with id newID, ReversesTransactionID pointing back at t.ID, and every
// posting negated (Money.Neg, overflow-checked) on the same account in the
// same currency, narrated "reversal of <t.ID>". Negating every posting
// preserves each currency group's zero sum (see Validate): a reversal of a
// balanced transaction is itself balanced, per currency, without
// BuildReversal needing to re-derive or re-check that invariant itself.
//
// newID is taken as given, not generated here: callers that want storage to
// assign an id (the normal path, mirroring how Post and Convert leave t.ID
// empty for CreateTransaction to fill in) pass "", and callers that need a
// specific id up front (tests, mainly) pass one.
//
// The only error BuildReversal can return is domain.ErrOverflow, from
// Money.Neg on a posting whose amount is math.MinInt64 (the one value with
// no representable negation); every other posting negates cleanly.
func (t Transaction) BuildReversal(newID string) (Transaction, error) {
	originalID := t.ID
	postings := make([]Posting, len(t.Postings))
	desc := reversalDescriptionPrefix + originalID
	if len(desc) > MaxPostingDescriptionLen {
		desc = desc[:MaxPostingDescriptionLen]
	}
	for i, p := range t.Postings {
		neg, err := p.Amount.Neg()
		if err != nil {
			return Transaction{}, err
		}
		postings[i] = Posting{AccountID: p.AccountID, Amount: neg, Description: desc}
	}
	return Transaction{
		ID:                    newID,
		Postings:              postings,
		ReversesTransactionID: &originalID,
	}, nil
}
