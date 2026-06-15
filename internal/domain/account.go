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
