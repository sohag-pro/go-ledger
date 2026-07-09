package domain

// AccountType is one of the five fundamental account classes in double-entry
// accounting. The zero value is intentionally invalid so an uninitialized
// AccountType fails Validate rather than masquerading as a real type.
type AccountType int

const (
	// Asset accounts hold what the entity owns; debit-normal.
	Asset AccountType = iota + 1
	// Liability accounts hold what the entity owes; credit-normal.
	Liability
	// Equity accounts hold owner residual interest; credit-normal.
	Equity
	// Income accounts record revenue earned; credit-normal.
	Income
	// Expense accounts record costs incurred; debit-normal.
	Expense
)

// Validate reports whether t is a defined account type.
func (t AccountType) Validate() error {
	switch t {
	case Asset, Liability, Equity, Income, Expense:
		return nil
	default:
		return ErrInvalidAccountType
	}
}

// IsDebitNormal reports whether the account's balance increases on the debit
// (positive) side. Assets and expenses are debit-normal; liabilities, equity,
// and income are credit-normal. An undefined type is treated as not
// debit-normal; callers should Validate first.
func (t AccountType) IsDebitNormal() bool {
	return t == Asset || t == Expense
}

// String returns the lowercase name, or "unknown" for an undefined type.
func (t AccountType) String() string {
	switch t {
	case Asset:
		return "asset"
	case Liability:
		return "liability"
	case Equity:
		return "equity"
	case Income:
		return "income"
	case Expense:
		return "expense"
	default:
		return "unknown"
	}
}

// ParseAccountType maps the lowercase name produced by String back to its
// AccountType. It returns ErrInvalidAccountType for any unrecognized name. This
// is the inverse of String and is used when reading a stored account type.
func ParseAccountType(s string) (AccountType, error) {
	switch s {
	case "asset":
		return Asset, nil
	case "liability":
		return Liability, nil
	case "equity":
		return Equity, nil
	case "income":
		return Income, nil
	case "expense":
		return Expense, nil
	default:
		return 0, ErrInvalidAccountType
	}
}

// Account is a named ledger account of a given type and currency. It is
// identity plus classification only; the balance is never stored here, it is
// derived by summing postings (see ADR-001).
//
// System marks an account as infrastructure rather than a user-owned account:
// the FX clearing accounts introduced in ADR-014 are the current example, one
// per tenant per currency, created lazily to absorb the open position of a
// cross-currency transaction. A system account still has a normal accounting
// type (an FX clearing account is a Liability), so no accounting rule
// changes; System only changes how the account is treated outside the
// balance invariant. System accounts are hidden from user-facing balance
// listings, and unlike a user account they are expected to carry a
// permanent, often negative, open position rather than settle to zero: that
// open position, revalued at current rates, is the tenant's FX exposure.
type Account struct {
	ID       string
	Name     string
	Type     AccountType
	Currency Currency
	System   bool
}

// IsSystem reports whether the account is a system account (for example an
// FX clearing account) rather than a user-owned account.
func (a Account) IsSystem() bool {
	return a.System
}

// Validate checks that the account has an id and name, a defined type, and a
// well-formed currency.
func (a Account) Validate() error {
	if a.ID == "" || a.Name == "" {
		return ErrInvalidAccount
	}
	if err := a.Type.Validate(); err != nil {
		return err
	}
	return a.Currency.Validate()
}
