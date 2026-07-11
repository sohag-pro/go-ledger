package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// CurrencyBalanceBody is one currency's net posted total in the trial
// balance report (Task 6.3, audit A9.2): the double-entry balance proof. In
// a correct ledger Imbalance is always false; a true value names a genuine
// invariant violation.
type CurrencyBalanceBody struct {
	Currency  string `json:"currency"`
	Net       int64  `json:"net" doc:"Sum of every posting in this currency across the tenant, in minor units. Must be zero in a correct double-entry ledger."`
	Imbalance bool   `json:"imbalance" doc:"true if net is nonzero, which should never happen"`
}

// AccountBalanceBody is one account's identity plus its derived balance in
// the trial balance report. IsSystem accounts (FX clearing accounts,
// ADR-014) are included, clearly marked, since they hold the FX position and
// are part of the balance proof.
type AccountBalanceBody struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Type      string `json:"type" doc:"One of: asset, liability, equity, income, expense"`
	Currency  string `json:"currency"`
	IsSystem  bool   `json:"is_system" doc:"true for a system account (e.g. an FX clearing account), which carries a permanent open position rather than a normal user-owned balance"`
	Balance   int64  `json:"balance" doc:"Signed derived balance in minor units"`
}

// TrialBalanceOutput is the GET /v1/reports/trial-balance response.
type TrialBalanceOutput struct {
	Body struct {
		Currencies []CurrencyBalanceBody `json:"currencies"`
		Accounts   []AccountBalanceBody  `json:"accounts"`
	}
}

func registerReports(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "trial-balance",
		Method:      http.MethodGet,
		Path:        "/v1/reports/trial-balance",
		Summary:     "Trial balance: the double-entry balance proof",
		Description: "Returns, for the caller's tenant, each currency's net posted total (which must be zero in a correct double-entry ledger, the balance proof) and every account's derived balance, including system (FX clearing) accounts, which hold the FX position and are part of the proof.",
		Tags:        []string{"reports"},
		Security:    bearerSecurity,
	}, func(ctx context.Context, _ *struct{}) (*TrialBalanceOutput, error) {
		tenant, err := tenantFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		tb, err := deps.Reports.TrialBalance(ctx, tenant)
		if err != nil {
			return nil, toHumaErr(err)
		}
		out := &TrialBalanceOutput{}
		out.Body.Currencies = make([]CurrencyBalanceBody, 0, len(tb.Currencies))
		for _, c := range tb.Currencies {
			out.Body.Currencies = append(out.Body.Currencies, CurrencyBalanceBody{
				Currency:  string(c.Currency),
				Net:       c.Net,
				Imbalance: c.Imbalance,
			})
		}
		out.Body.Accounts = make([]AccountBalanceBody, 0, len(tb.Accounts))
		for _, a := range tb.Accounts {
			out.Body.Accounts = append(out.Body.Accounts, AccountBalanceBody{
				AccountID: a.AccountID,
				Name:      a.Name,
				Type:      a.Type.String(),
				Currency:  string(a.Currency),
				IsSystem:  a.IsSystem,
				Balance:   a.Balance,
			})
		}
		return out, nil
	})
}
