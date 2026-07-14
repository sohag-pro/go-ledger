package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

// MaxReportRows bounds an unpaged whole-tenant read (the account tree and the
// trial balance): the underlying queries LIMIT at MaxReportRows+1 so the
// service can detect an over-large result and return domain.ErrReportTooLarge
// rather than build an unbounded response in memory (audit remediation: bound
// the unbounded reads). Keep in sync with the LIMIT literals in
// queries/accounts.sql (AllAccountBalances) and queries/reports.sql
// (TrialBalanceAccounts).
const MaxReportRows = 10000

// CurrencyBalance is one currency's net posted total across a tenant's
// postings, plus whether it is an imbalance (Task 6.3, audit A9.2): the
// per-currency half of TrialBalance. See domain.CurrencyTotal for the raw
// repository read this wraps.
type CurrencyBalance struct {
	Currency domain.Currency
	Net      int64
	// Imbalance is true when Net is nonzero: in a correct double-entry
	// ledger this never happens (ADR-001), so a caller surfacing Imbalance
	// true is reporting a genuine invariant violation, not a normal state.
	Imbalance bool
}

// TrialBalanceAccount is one account's identity, derived balance, and
// (ADR-023) the balance rolled up over its whole subtree, embedding
// domain.AccountBalance so existing field access (a.AccountID, a.Balance,
// a.Name, a.Type, a.Currency, a.IsSystem) keeps working unchanged.
// RolledUpBalance is additive display only: it double-counts a parent and
// its descendants on purpose, so it must never be used in place of Balance
// for the balance-proof check below, which stays computed from own balances.
type TrialBalanceAccount struct {
	domain.AccountBalance
	RolledUpBalance int64
}

// TrialBalance is a tenant's full balance proof (Task 6.3, audit A9.2): each
// currency's net posted total (Currencies, which must each be zero) and
// every account's derived balance (Accounts, including system/clearing
// accounts), so a caller can see both that the books balance and where the
// value actually sits.
type TrialBalance struct {
	Currencies []CurrencyBalance
	Accounts   []TrialBalanceAccount
}

// ReportService is the application service backing GET
// /v1/reports/trial-balance (Task 6.3, audit A9.2). Like AccountService it is
// thin: it holds no SQL, just assembling the repository's two raw reads into
// one report.
type ReportService struct {
	repo domain.Repository
}

// NewReportService returns a ReportService backed by repo.
func NewReportService(repo domain.Repository) *ReportService {
	return &ReportService{repo: repo}
}

// TrialBalance returns tenantID's full balance proof: each currency's net
// posted total (flagged as an imbalance if nonzero) and every account's
// derived balance, plus (ADR-023) each account's balance rolled up over its
// subtree. The per-currency nets above are computed from TrialBalanceByCurrency
// (own balances only, unchanged): rollups double-count a parent and its
// descendants by design, so they stay a display-only addition on Accounts and
// never feed the zero-proof.
func (s *ReportService) TrialBalance(ctx context.Context, tenantID string) (TrialBalance, error) {
	totals, err := s.repo.TrialBalanceByCurrency(ctx, tenantID)
	if err != nil {
		return TrialBalance{}, err
	}
	accounts, err := s.repo.TrialBalanceAccounts(ctx, tenantID)
	if err != nil {
		return TrialBalance{}, err
	}
	if len(accounts) > MaxReportRows {
		return TrialBalance{}, domain.ErrReportTooLarge
	}
	// Reuse the same tree rollup Tree/AccountService.buildTree uses: one
	// query (AllAccountBalances) covers the same account universe
	// TrialBalanceAccounts does (both include system/clearing accounts), so
	// building the rollup map here costs nothing beyond that one extra read.
	rows, err := s.repo.AllAccountBalances(ctx, tenantID)
	if err != nil {
		return TrialBalance{}, err
	}
	rolledUp := make(map[string]int64, len(rows))
	for _, n := range buildTree(rows) {
		rolledUp[n.Account.ID] = n.RolledUpBalance
	}
	currencies := make([]CurrencyBalance, 0, len(totals))
	for _, t := range totals {
		currencies = append(currencies, CurrencyBalance{
			Currency:  t.Currency,
			Net:       t.Net,
			Imbalance: t.Net != 0,
		})
	}
	accountRows := make([]TrialBalanceAccount, 0, len(accounts))
	for _, a := range accounts {
		accountRows = append(accountRows, TrialBalanceAccount{
			AccountBalance:  a,
			RolledUpBalance: rolledUp[a.AccountID],
		})
	}
	return TrialBalance{Currencies: currencies, Accounts: accountRows}, nil
}
