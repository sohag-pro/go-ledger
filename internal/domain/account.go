package domain

import "fmt"

// MaxPartyReferenceLen and MaxPartyTypeLen bound Account.PartyReference and
// Account.PartyType (Task 6.1, audit A9.1): the same 256-character ceiling
// MaxPostingDescriptionLen and MaxTransactionReferenceLen already use for a
// free-form, client-supplied identifier. There is nothing special about the
// number itself, just an established sane cap.
const (
	MaxPartyReferenceLen = 256
	MaxPartyTypeLen      = 256
)

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
	// Status is the account's lifecycle gate (Task 5.5, audit A1.5): see
	// AccountStatus. The zero value ("") is treated as AccountActive
	// everywhere this field is read (CheckAccountPostingConstraints,
	// Validate below): every caller that predates this field, and every
	// existing test fixture that builds an Account literal without setting
	// it, keeps behaving exactly as it did before this field existed. A real
	// Postgres row always carries a concrete value (the column is NOT NULL
	// DEFAULT 'active', migration 0022), so "" only ever occurs for an
	// in-memory Account that has not round-tripped through storage.
	Status AccountStatus
	// MinBalance is an optional floor on the account's derived balance, in
	// the account's own minor units (Task 5.5, audit A1.5). nil means no
	// floor (unconstrained, the behavior of every account before this field
	// existed). A negative value is a legitimate overdraft allowance, not an
	// error: see migration 0022's doc comment.
	MinBalance *int64
	// PartyReference is an optional external customer/party id (Task 6.1,
	// audit A9.1): linkage metadata so an external KYC/customer system can
	// tie this account back to a party record it owns. nil means no linkage
	// was supplied, the default, and behaves exactly as every account did
	// before this field existed. This package does not validate the id
	// against anything: the party/KYC system is external and out of scope,
	// so beyond the length cap (MaxPartyReferenceLen) this is opaque,
	// free-form text.
	PartyReference *string
	// PartyType is an optional free-text classification of the linked party
	// (Task 6.1, audit A9.1), for example "individual" or "business". nil
	// means unset. Like PartyReference this is linkage metadata only: no
	// enum is enforced here (a real KYC system's taxonomy is external and
	// may not match a fixed set this package would have to keep in sync
	// with), beyond the length cap (MaxPartyTypeLen).
	PartyType *string
}

// IsSystem reports whether the account is a system account (for example an
// FX clearing account) rather than a user-owned account.
func (a Account) IsSystem() bool {
	return a.System
}

// Validate checks that the account has an id and name, a defined type, a
// well-formed currency, and (when set) a recognized Status. An empty Status
// is valid: see Account.Status's doc comment for why "unset" is not the same
// as "invalid" for this field. PartyReference and PartyType (Task 6.1, audit
// A9.1) are checked only against their length caps: they are opaque linkage
// metadata for an external KYC/party system, so there is no format or
// taxonomy for this package to enforce beyond that.
func (a Account) Validate() error {
	if a.ID == "" || a.Name == "" {
		return ErrInvalidAccount
	}
	if err := a.Type.Validate(); err != nil {
		return err
	}
	if a.Status != "" && !a.Status.Valid() {
		return ErrInvalidAccount
	}
	if a.PartyReference != nil && len(*a.PartyReference) > MaxPartyReferenceLen {
		return ErrPartyReferenceTooLong
	}
	if a.PartyType != nil && len(*a.PartyType) > MaxPartyTypeLen {
		return ErrPartyTypeTooLong
	}
	return a.Currency.Validate()
}

// AccountStatus is an account's lifecycle gate (Task 5.5, audit A1.5),
// mirroring TenantStatus's two-gate shape (internal/domain/tenant.go,
// ADR-015) one level down: a tenant-wide suspension gates every account at
// once, while AccountStatus gates one account at a time (a compliance hold
// on a single account, for example). Only these three values are valid; the
// zero value is handled as "unset" (treated as AccountActive), not as a
// fourth, invalid state: see Account.Status's doc comment.
type AccountStatus string

const (
	// AccountActive is the only status that may post (subject to
	// MinBalance): a non-system account in either other status is rejected
	// at the posting transaction (see CheckAccountPostingConstraints), not
	// by any change to its data.
	AccountActive AccountStatus = "active"
	// AccountFrozen is a temporary hold: an operator can reactivate a frozen
	// account by setting its status back to active (POST
	// /v1/accounts/{id}/status).
	AccountFrozen AccountStatus = "frozen"
	// AccountClosed is a gate this package does not expect to reverse, the
	// same convention TenantClosed follows; it is otherwise handled
	// identically to frozen.
	AccountClosed AccountStatus = "closed"
)

// Valid reports whether s is one of the three defined statuses. The zero
// value ("") reports false here (it is not itself one of the three names),
// but callers that read an Account's Status field treat "" as
// AccountActive rather than calling Valid on it directly: see
// CheckAccountPostingConstraints.
func (s AccountStatus) Valid() bool {
	switch s {
	case AccountActive, AccountFrozen, AccountClosed:
		return true
	default:
		return false
	}
}

// AccountNotActiveError is returned when a posting touches a non-system
// account whose Status is not AccountActive (Task 5.5, audit A1.5). Status
// carries the account's real status (frozen or closed) so a transport layer
// can name the exact reason instead of a generic message, the same shape
// TenantNotActiveError already gives one level up. It wraps
// ErrAccountNotActive so a caller can match with
// errors.Is(err, ErrAccountNotActive) regardless of which status applied.
type AccountNotActiveError struct {
	AccountID string
	Status    AccountStatus
}

// Error implements the error interface with a message naming the account
// and its exact status.
func (e *AccountNotActiveError) Error() string {
	return fmt.Sprintf("domain: account %s is %s", e.AccountID, e.Status)
}

// Unwrap lets errors.Is(err, ErrAccountNotActive) match regardless of which
// status caused it.
func (e *AccountNotActiveError) Unwrap() error { return ErrAccountNotActive }

// MinBalanceBreachError is returned when a posting would take a non-system
// account's balance below its configured MinBalance (Task 5.5, audit A1.5).
// It wraps ErrMinBalanceBreach so a caller can match with
// errors.Is(err, ErrMinBalanceBreach) without parsing the message.
type MinBalanceBreachError struct {
	AccountID  string
	MinBalance int64
	// NewBalance is the balance the posting would have produced: the
	// account's current derived balance plus this transaction's own signed
	// total for the account, in the account's currency.
	NewBalance int64
}

// Error implements the error interface with a message naming the account,
// the balance the posting would have produced, and the floor it breaches.
func (e *MinBalanceBreachError) Error() string {
	return fmt.Sprintf("domain: posting would take account %s to %d, below its minimum balance %d", e.AccountID, e.NewBalance, e.MinBalance)
}

// Unwrap lets errors.Is(err, ErrMinBalanceBreach) match regardless of which
// account or floor caused it.
func (e *MinBalanceBreachError) Unwrap() error { return ErrMinBalanceBreach }

// AccountPostingState is one account's current Status, optional MinBalance,
// System flag, and derived Balance, as read INSIDE the posting transaction
// (Task 5.5, audit A1.5): see domain.Tx.AccountPostingStates, whose doc
// comment explains why this read must happen on the transaction's own
// connection, under its own SERIALIZABLE isolation, rather than through the
// plain Repository.
type AccountPostingState struct {
	AccountID string
	Status    AccountStatus
	// MinBalance is nil when the account has no floor configured.
	MinBalance *int64
	IsSystem   bool
	// Balance is the account's derived balance BEFORE this transaction's own
	// postings are applied: CheckAccountPostingConstraints adds this
	// transaction's own per-account total to it before comparing against
	// MinBalance.
	Balance int64
}

// CheckAccountPostingConstraints enforces, for every NON-system account
// touched by postings, that the account's Status is AccountActive (or
// unset, see Account.Status's doc comment) and that posting would not take
// its balance below its optional MinBalance (Task 5.5, audit A1.5).
//
// states must carry an entry for every distinct account id postings
// touches (see domain.Tx.AccountPostingStates); a postings entry with no
// matching states entry is treated as ErrAccountNotFound, the same error a
// stale or cross-tenant account id would already produce further down the
// posting path, rather than silently skipping the check.
//
// A system account (IsSystem true: the FX clearing accounts, ADR-014) is
// exempt from BOTH checks: it is expected to carry a permanent, often
// negative, open position, never a normal user-owned balance floor or
// freeze.
//
// Only the account's OWN currency is ever summed (an account has exactly
// one currency, ADR-014), so postings.Amount.Amount() is added directly
// without any per-currency grouping: unlike CheckTransactionPolicy (which
// must group by currency because a single transaction can span two), a
// single account can never be touched in two different currencies within
// one transaction (the database enforces postings.currency = the account's
// currency), so every posting for a given account id already shares one
// currency by construction.
func CheckAccountPostingConstraints(states map[string]AccountPostingState, postings []Posting) error {
	deltas := make(map[string]int64, len(postings))
	for _, p := range postings {
		deltas[p.AccountID] += p.Amount.Amount()
	}
	for accountID, delta := range deltas {
		state, ok := states[accountID]
		if !ok {
			return ErrAccountNotFound
		}
		if state.IsSystem {
			continue
		}
		if state.Status != "" && state.Status != AccountActive {
			return &AccountNotActiveError{AccountID: accountID, Status: state.Status}
		}
		if state.MinBalance != nil {
			newBalance := state.Balance + delta
			if newBalance < *state.MinBalance {
				return &MinBalanceBreachError{AccountID: accountID, MinBalance: *state.MinBalance, NewBalance: newBalance}
			}
		}
	}
	return nil
}
