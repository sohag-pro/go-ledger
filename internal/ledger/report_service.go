package ledger

import (
	"context"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

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

// TrialBalance is a tenant's full balance proof (Task 6.3, audit A9.2): each
// currency's net posted total (Currencies, which must each be zero) and
// every account's derived balance (Accounts, including system/clearing
// accounts), so a caller can see both that the books balance and where the
// value actually sits.
type TrialBalance struct {
	Currencies []CurrencyBalance
	Accounts   []domain.AccountBalance
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
// derived balance.
func (s *ReportService) TrialBalance(ctx context.Context, tenantID string) (TrialBalance, error) {
	totals, err := s.repo.TrialBalanceByCurrency(ctx, tenantID)
	if err != nil {
		return TrialBalance{}, err
	}
	accounts, err := s.repo.TrialBalanceAccounts(ctx, tenantID)
	if err != nil {
		return TrialBalance{}, err
	}
	currencies := make([]CurrencyBalance, 0, len(totals))
	for _, t := range totals {
		currencies = append(currencies, CurrencyBalance{
			Currency:  t.Currency,
			Net:       t.Net,
			Imbalance: t.Net != 0,
		})
	}
	return TrialBalance{Currencies: currencies, Accounts: accounts}, nil
}
