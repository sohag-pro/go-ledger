package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestTrialBalance checks that the report's per-currency net is zero after a
// balanced post, and that both touched accounts show up with their correct
// derived balances.
func TestTrialBalance(t *testing.T) {
	r := newAPIRouter(newFakeRepo())
	cash := createAccount(t, r, "Cash", "asset")
	rev := createAccount(t, r, "Revenue", "income")
	postTxn(t, r, cash, rev, 7500, "trial-balance-1")

	rec := do(t, r, http.MethodGet, "/v1/reports/trial-balance", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out TrialBalanceOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(out.Body.Currencies) != 1 {
		t.Fatalf("currencies = %+v, want exactly one (USD)", out.Body.Currencies)
	}
	usd := out.Body.Currencies[0]
	if usd.Currency != "USD" {
		t.Errorf("currency = %q, want %q", usd.Currency, "USD")
	}
	if usd.Net != 0 {
		t.Errorf("USD net = %d, want 0 (the balance proof)", usd.Net)
	}
	if usd.Imbalance {
		t.Error("imbalance = true, want false")
	}

	balances := map[string]int64{}
	for _, a := range out.Body.Accounts {
		balances[a.AccountID] = a.Balance
	}
	if balances[cash] != 7500 {
		t.Errorf("cash balance in report = %d, want 7500", balances[cash])
	}
	if balances[rev] != -7500 {
		t.Errorf("revenue balance in report = %d, want -7500", balances[rev])
	}
}
