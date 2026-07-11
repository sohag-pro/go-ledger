package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// TenantStatus is a tenant's lifecycle state (ADR-015, Task 2.1: the
// white-label MVP's foundation). Only these three values are valid; every
// other string, including the zero value, is not.
type TenantStatus string

const (
	// TenantActive is the only status that may post or read: a tenant in
	// either other status is gated out at the auth boundary (internal/auth),
	// not by any change to its data.
	TenantActive TenantStatus = "active"
	// TenantSuspended is a temporary gate: an operator can reactivate a
	// suspended tenant by setting its status back to active.
	TenantSuspended TenantStatus = "suspended"
	// TenantClosed is a gate an operator does not expect to reverse. It is
	// otherwise handled identically to suspended: this package does not
	// enforce that a closed tenant can never return to active.
	TenantClosed TenantStatus = "closed"
)

// Valid reports whether s is one of the three defined statuses.
func (s TenantStatus) Valid() bool {
	switch s {
	case TenantActive, TenantSuspended, TenantClosed:
		return true
	default:
		return false
	}
}

// Tenant is a first-class tenant row: the entity an operator suspends or
// closes. Settings is opaque here (its shape is populated in Task 2.4); this
// package only carries it through as raw JSON.
type Tenant struct {
	ID        string
	Name      string
	Status    TenantStatus
	Settings  json.RawMessage
	CreatedAt time.Time
}

// Validate reports whether t is well-formed: a non-empty Name and a valid
// Status. It does not check ID: like Account and Transaction, an empty ID
// means the storage adapter assigns one.
func (t Tenant) Validate() error {
	if t.Name == "" {
		return ErrInvalidTenant
	}
	if !t.Status.Valid() {
		return ErrInvalidTenant
	}
	return nil
}

// TenantNotActiveError is returned when a request is scoped to a tenant whose
// status is not TenantActive. Status carries the tenant's real status
// (suspended or closed) so the transport layer can name the exact reason
// instead of a generic message. It wraps ErrTenantNotActive so callers can
// match with errors.Is(err, ErrTenantNotActive) without caring which status
// applied.
type TenantNotActiveError struct {
	TenantID string
	Status   TenantStatus
}

// Error implements the error interface. It deliberately does not start with
// "domain:" like the package's sentinel errors: it is meant to be read
// directly by an operator or logged as-is, not just matched against.
func (e *TenantNotActiveError) Error() string {
	return "tenant " + e.TenantID + " is " + string(e.Status)
}

// Unwrap lets errors.Is(err, ErrTenantNotActive) match regardless of which
// status caused it.
func (e *TenantNotActiveError) Unwrap() error { return ErrTenantNotActive }

// Reason returns the caller-facing explanation for why the tenant is gated,
// naming the exact status (e.g. "tenant is suspended"). This is what a
// transport layer should put in a 403 / PermissionDenied response body: it
// names the reason without leaking the tenant id to an unauthenticated or
// cross-tenant caller.
func (e *TenantNotActiveError) Reason() string {
	return "tenant is " + string(e.Status)
}

// TenantPolicy is optional per-tenant guardrails on the posting path (Task
// 2.4b, audit A3.4): a max single-transaction amount, a daily volume cap, and
// a currency allowlist. A zero/empty field means no limit, so a tenant with a
// zero-value TenantPolicy (the common case: no policy ever configured) posts
// exactly as it did before this feature existed.
//
// All amounts are in a currency's minor units (the same unit every other
// amount in this codebase uses) and are evaluated PER CURRENCY: the ledger is
// multi-currency (ADR-014), and a USD total and a EUR total can never be
// meaningfully summed together, so every check below groups by currency
// rather than combining them. See CheckTransactionPolicy.
type TenantPolicy struct {
	// MaxTransactionAmount caps a single transaction's per-currency debit
	// total. 0 means unlimited.
	MaxTransactionAmount int64 `json:"max_transaction_amount,omitempty"`
	// DailyVolumeLimit caps a tenant's cumulative per-currency debit total
	// for the current day (today's already-posted total plus this
	// transaction's own total). 0 means unlimited.
	DailyVolumeLimit int64 `json:"daily_volume_limit,omitempty"`
	// AllowedCurrencies restricts which currencies a posting may use. Empty
	// means every currency is allowed.
	AllowedCurrencies []string `json:"allowed_currencies,omitempty"`
}

// Validate reports ErrInvalidTenantPolicy if either amount limit is negative
// or any entry of AllowedCurrencies is not a well-formed three-letter
// currency code. It does not check the ISO 4217 registry, the same shape-only
// rule Currency.Validate applies everywhere else in this package.
func (p TenantPolicy) Validate() error {
	if p.MaxTransactionAmount < 0 {
		return ErrInvalidTenantPolicy
	}
	if p.DailyVolumeLimit < 0 {
		return ErrInvalidTenantPolicy
	}
	for _, c := range p.AllowedCurrencies {
		if err := Currency(c).Validate(); err != nil {
			return ErrInvalidTenantPolicy
		}
	}
	return nil
}

// isZero reports whether p carries no guardrails at all: every check in
// CheckTransactionPolicy is already a no-op for a zero-value field, so this
// only exists as a fast, explicit way for a caller (the ledger service) to
// skip a daily-volume read entirely when there is nothing to enforce, rather
// than as a correctness requirement of CheckTransactionPolicy itself.
func (p TenantPolicy) isZero() bool {
	return p.MaxTransactionAmount == 0 && p.DailyVolumeLimit == 0 && len(p.AllowedCurrencies) == 0
}

// TenantSettings is the decoded shape of the tenants.settings jsonb column
// (added in Task 2.1, opaque on Tenant until this task gave it a shape).
// Policy is the only field today; the jsonb column is left room to grow
// (e.g. webhook config) without another migration.
type TenantSettings struct {
	Policy TenantPolicy `json:"policy"`
}

// ParseTenantSettings decodes raw (the tenants.settings column) into a
// TenantSettings. An empty or all-whitespace raw value (including a tenant
// row that predates any settings ever being written) decodes to the zero
// value with no error, not a JSON parse error: "no settings" and "{}" mean
// exactly the same thing, an empty TenantPolicy with no guardrails.
func ParseTenantSettings(raw json.RawMessage) (TenantSettings, error) {
	var s TenantSettings
	if len(bytes.TrimSpace(raw)) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return TenantSettings{}, fmt.Errorf("domain: parse tenant settings: %w", err)
	}
	return s, nil
}

// PolicyRule names which TenantPolicy guardrail a transaction tripped, so a
// PolicyViolationError (and the transport-layer error body built from it) can
// say exactly which rule fired instead of a bare "policy violation".
type PolicyRule string

const (
	// PolicyRuleCurrencyNotAllowed fires when a posting's currency is not in
	// a non-empty TenantPolicy.AllowedCurrencies.
	PolicyRuleCurrencyNotAllowed PolicyRule = "currency_not_allowed"
	// PolicyRuleMaxTransactionAmount fires when a single currency's debit
	// total within one transaction exceeds TenantPolicy.MaxTransactionAmount.
	PolicyRuleMaxTransactionAmount PolicyRule = "max_transaction_amount"
	// PolicyRuleDailyVolumeLimit fires when a currency's already-posted
	// today total plus this transaction's own total would exceed
	// TenantPolicy.DailyVolumeLimit.
	PolicyRuleDailyVolumeLimit PolicyRule = "daily_volume_limit"
)

// PolicyViolationError is returned when a transaction trips one of a
// tenant's TenantPolicy guardrails (Task 2.4b, audit A3.4). It carries enough
// detail (which rule, which currency, the amount and the limit) for a
// transport layer to build a clear, actionable 422/FailedPrecondition body
// instead of a bare "policy violation" message. It wraps ErrPolicyViolation
// so a caller that only cares "was this rejected by policy" can match with
// errors.Is(err, ErrPolicyViolation) regardless of which rule fired.
type PolicyViolationError struct {
	Rule     PolicyRule
	Currency Currency
	// Amount is the total that tripped the rule: the offending transaction's
	// own per-currency debit total for PolicyRuleMaxTransactionAmount, or the
	// projected today-plus-this-transaction total for
	// PolicyRuleDailyVolumeLimit. It is 0 (unused) for
	// PolicyRuleCurrencyNotAllowed.
	Amount int64
	// Limit is the TenantPolicy value that was exceeded. It is 0 (unused)
	// for PolicyRuleCurrencyNotAllowed.
	Limit int64
}

// Error implements the error interface with a message naming the exact rule
// and currency, and (where relevant) the amount and limit involved.
func (e *PolicyViolationError) Error() string {
	switch e.Rule {
	case PolicyRuleCurrencyNotAllowed:
		return fmt.Sprintf("domain: currency %s is not allowed by tenant policy", e.Currency)
	case PolicyRuleMaxTransactionAmount:
		return fmt.Sprintf("domain: %s transaction debit total %d exceeds tenant max transaction amount %d", e.Currency, e.Amount, e.Limit)
	case PolicyRuleDailyVolumeLimit:
		return fmt.Sprintf("domain: %s debit total %d would exceed tenant daily volume limit %d", e.Currency, e.Amount, e.Limit)
	default:
		return fmt.Sprintf("domain: tenant policy violation (%s, %s)", e.Rule, e.Currency)
	}
}

// Unwrap lets errors.Is(err, ErrPolicyViolation) match regardless of which
// rule caused it.
func (e *PolicyViolationError) Unwrap() error { return ErrPolicyViolation }

// CheckTransactionPolicy evaluates postings against policy's guardrails
// (Task 2.4b, audit A3.4), grouped and checked independently PER CURRENCY:
// the ledger is multi-currency (ADR-014), so a USD total and a EUR total are
// never summed together. A zero-value policy always passes: every check
// below is a no-op when its governing field is its zero value, so a tenant
// with no policy configured is completely unaffected.
//
// Only the DEBIT (positive amount, ADR-002) total per currency is checked.
// Transaction.Validate already guarantees each currency's postings sum to
// zero, so a currency's credits are exactly the mirror image of its debits;
// checking one side is enough and avoids double-counting the same movement.
//
// dailyDebits carries each currency's already-posted debit total for "today"
// (see domain.Tx.TenantDailyDebits), keyed by currency code; a currency
// absent from the map is treated as 0. It may be nil when policy has no
// DailyVolumeLimit, since the caller then has no reason to have read it.
//
// The allowlist is checked before any total is accumulated: a posting in a
// disallowed currency is rejected even if that currency's amount would
// otherwise pass every other rule.
func CheckTransactionPolicy(policy TenantPolicy, postings []Posting, dailyDebits map[string]int64) error {
	if policy.isZero() {
		return nil
	}
	debitTotals := make(map[Currency]int64, len(postings))
	for _, p := range postings {
		cur := p.Amount.Currency()
		if len(policy.AllowedCurrencies) > 0 && !currencyInList(policy.AllowedCurrencies, cur) {
			return &PolicyViolationError{Rule: PolicyRuleCurrencyNotAllowed, Currency: cur}
		}
		if amt := p.Amount.Amount(); amt > 0 {
			debitTotals[cur] += amt
		}
	}
	for cur, total := range debitTotals {
		if policy.MaxTransactionAmount > 0 && total > policy.MaxTransactionAmount {
			return &PolicyViolationError{
				Rule: PolicyRuleMaxTransactionAmount, Currency: cur,
				Amount: total, Limit: policy.MaxTransactionAmount,
			}
		}
		if policy.DailyVolumeLimit > 0 {
			projected := dailyDebits[string(cur)] + total
			if projected > policy.DailyVolumeLimit {
				return &PolicyViolationError{
					Rule: PolicyRuleDailyVolumeLimit, Currency: cur,
					Amount: projected, Limit: policy.DailyVolumeLimit,
				}
			}
		}
	}
	return nil
}

// currencyInList reports whether cur appears in allowed.
func currencyInList(allowed []string, cur Currency) bool {
	for _, a := range allowed {
		if Currency(a) == cur {
			return true
		}
	}
	return false
}
