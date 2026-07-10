package domain

// CurrencyTotal is one currency's net posted total across every account in a
// tenant (Task 6.3, audit A9.2): SUM(amount) over every posting in that
// currency. In a correct double-entry ledger this is always zero (the
// balance proof, ADR-001); a nonzero total means the invariant has been
// violated somewhere and should never happen in production.
type CurrencyTotal struct {
	Currency Currency
	Net      int64
}

// AccountBalance is one account's identity plus its derived balance (Task
// 6.3, audit A9.2): the per-account half of the trial balance report, so a
// caller can see where the tenant's value actually sits, not just that the
// currency totals net to zero. IsSystem accounts (FX clearing accounts,
// ADR-014) are included, clearly marked, since they hold the FX position and
// are part of the balance proof.
type AccountBalance struct {
	AccountID string
	Name      string
	Type      AccountType
	Currency  Currency
	IsSystem  bool
	Balance   int64
}
