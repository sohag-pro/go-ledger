package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestTenantStatus_Valid(t *testing.T) {
	tests := []struct {
		status TenantStatus
		want   bool
	}{
		{TenantActive, true},
		{TenantSuspended, true},
		{TenantClosed, true},
		{"", false},
		{"ACTIVE", false},
		{"pending", false},
		{"active ", false},
	}
	for _, tt := range tests {
		if got := tt.status.Valid(); got != tt.want {
			t.Errorf("TenantStatus(%q).Valid() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestTenant_Validate(t *testing.T) {
	tests := []struct {
		name    string
		tenant  Tenant
		wantErr bool
	}{
		{"valid active tenant", Tenant{Name: "Acme Corp", Status: TenantActive}, false},
		{"valid suspended tenant", Tenant{Name: "Acme Corp", Status: TenantSuspended}, false},
		{"valid closed tenant", Tenant{Name: "Acme Corp", Status: TenantClosed}, false},
		{"empty name", Tenant{Name: "", Status: TenantActive}, true},
		{"invalid status", Tenant{Name: "Acme Corp", Status: "pending"}, true},
		{"empty status", Tenant{Name: "Acme Corp", Status: ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tenant.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidTenant) {
				t.Errorf("Validate() = %v, want ErrInvalidTenant", err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestTenantNotActiveError(t *testing.T) {
	err := &TenantNotActiveError{TenantID: "tenant-1", Status: TenantSuspended}

	if !errors.Is(err, ErrTenantNotActive) {
		t.Error("TenantNotActiveError does not match ErrTenantNotActive via errors.Is")
	}
	wantReason := "tenant is suspended"
	if got := err.Reason(); got != wantReason {
		t.Errorf("Reason() = %q, want %q", got, wantReason)
	}
	if err.Error() == "" {
		t.Error("Error() returned an empty string")
	}

	closedErr := &TenantNotActiveError{TenantID: "tenant-2", Status: TenantClosed}
	if got := closedErr.Reason(); got != "tenant is closed" {
		t.Errorf("Reason() for closed = %q, want %q", got, "tenant is closed")
	}
}

func TestParseTenantSettings(t *testing.T) {
	tests := []struct {
		name    string
		raw     json.RawMessage
		want    TenantSettings
		wantErr bool
	}{
		{"nil raw", nil, TenantSettings{}, false},
		{"empty raw", json.RawMessage(""), TenantSettings{}, false},
		{"whitespace only", json.RawMessage("   \n"), TenantSettings{}, false},
		{"empty object", json.RawMessage("{}"), TenantSettings{}, false},
		{"json null", json.RawMessage("null"), TenantSettings{}, false},
		{
			"populated policy",
			json.RawMessage(`{"policy":{"max_transaction_amount":10000,"daily_volume_limit":50000,"allowed_currencies":["USD","EUR"]}}`),
			TenantSettings{Policy: TenantPolicy{
				MaxTransactionAmount: 10000,
				DailyVolumeLimit:     50000,
				AllowedCurrencies:    []string{"USD", "EUR"},
			}},
			false,
		},
		{"malformed json", json.RawMessage(`{"policy":`), TenantSettings{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTenantSettings(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseTenantSettings() = nil error, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTenantSettings() unexpected error: %v", err)
			}
			if got.Policy.MaxTransactionAmount != tt.want.Policy.MaxTransactionAmount ||
				got.Policy.DailyVolumeLimit != tt.want.Policy.DailyVolumeLimit ||
				len(got.Policy.AllowedCurrencies) != len(tt.want.Policy.AllowedCurrencies) {
				t.Errorf("ParseTenantSettings() = %+v, want %+v", got, tt.want)
			}
			for i, c := range tt.want.Policy.AllowedCurrencies {
				if got.Policy.AllowedCurrencies[i] != c {
					t.Errorf("ParseTenantSettings() allowed_currencies[%d] = %q, want %q", i, got.Policy.AllowedCurrencies[i], c)
				}
			}
		})
	}
}

func TestTenantPolicy_Validate(t *testing.T) {
	tests := []struct {
		name    string
		policy  TenantPolicy
		wantErr bool
	}{
		{"zero value", TenantPolicy{}, false},
		{"valid limits and currencies", TenantPolicy{MaxTransactionAmount: 100, DailyVolumeLimit: 1000, AllowedCurrencies: []string{"USD", "EUR"}}, false},
		{"negative max transaction amount", TenantPolicy{MaxTransactionAmount: -1}, true},
		{"negative daily volume limit", TenantPolicy{DailyVolumeLimit: -1}, true},
		{"malformed currency code", TenantPolicy{AllowedCurrencies: []string{"US"}}, true},
		{"lowercase currency code", TenantPolicy{AllowedCurrencies: []string{"usd"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidTenantPolicy) {
				t.Errorf("Validate() = %v, want ErrInvalidTenantPolicy", err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// mustPosting builds a Posting for the given account/amount/currency,
// failing the test immediately on an invalid currency: every case in this
// file's policy tests uses well-formed currencies, so a construction error
// here would mean the test itself is broken, not the code under test.
func mustPosting(t *testing.T, accountID string, amount int64, currency Currency) Posting {
	t.Helper()
	m, err := NewMoney(amount, currency)
	if err != nil {
		t.Fatalf("NewMoney(%d, %q): %v", amount, currency, err)
	}
	return Posting{AccountID: accountID, Amount: m}
}

func TestCheckTransactionPolicy_ZeroPolicyAlwaysPasses(t *testing.T) {
	postings := []Posting{
		mustPosting(t, "a", 1_000_000, "USD"),
		mustPosting(t, "b", -1_000_000, "USD"),
	}
	if err := CheckTransactionPolicy(TenantPolicy{}, postings, nil); err != nil {
		t.Errorf("CheckTransactionPolicy() with zero policy = %v, want nil", err)
	}
}

func TestCheckTransactionPolicy_AllowedCurrencies(t *testing.T) {
	policy := TenantPolicy{AllowedCurrencies: []string{"USD"}}

	allowed := []Posting{
		mustPosting(t, "a", 100, "USD"),
		mustPosting(t, "b", -100, "USD"),
	}
	if err := CheckTransactionPolicy(policy, allowed, nil); err != nil {
		t.Errorf("allowed currency rejected: %v", err)
	}

	disallowed := []Posting{
		mustPosting(t, "a", 100, "EUR"),
		mustPosting(t, "b", -100, "EUR"),
	}
	err := CheckTransactionPolicy(policy, disallowed, nil)
	if !errors.Is(err, ErrPolicyViolation) {
		t.Fatalf("disallowed currency: err = %v, want ErrPolicyViolation", err)
	}
	var pv *PolicyViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("err is not *PolicyViolationError: %v", err)
	}
	if pv.Rule != PolicyRuleCurrencyNotAllowed || pv.Currency != "EUR" {
		t.Errorf("PolicyViolationError = %+v, want Rule=%s Currency=EUR", pv, PolicyRuleCurrencyNotAllowed)
	}
	if pv.Error() == "" {
		t.Error("Error() returned empty string")
	}
}

func TestCheckTransactionPolicy_MaxTransactionAmount(t *testing.T) {
	policy := TenantPolicy{MaxTransactionAmount: 1000}

	underCap := []Posting{
		mustPosting(t, "a", 999, "USD"),
		mustPosting(t, "b", -999, "USD"),
	}
	if err := CheckTransactionPolicy(policy, underCap, nil); err != nil {
		t.Errorf("under cap rejected: %v", err)
	}

	atCap := []Posting{
		mustPosting(t, "a", 1000, "USD"),
		mustPosting(t, "b", -1000, "USD"),
	}
	if err := CheckTransactionPolicy(policy, atCap, nil); err != nil {
		t.Errorf("exactly at cap rejected: %v", err)
	}

	overCap := []Posting{
		mustPosting(t, "a", 1001, "USD"),
		mustPosting(t, "b", -1001, "USD"),
	}
	err := CheckTransactionPolicy(policy, overCap, nil)
	var pv *PolicyViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("over cap: err = %v, want *PolicyViolationError", err)
	}
	if pv.Rule != PolicyRuleMaxTransactionAmount || pv.Currency != "USD" || pv.Amount != 1001 || pv.Limit != 1000 {
		t.Errorf("PolicyViolationError = %+v, want Rule=%s Currency=USD Amount=1001 Limit=1000", pv, PolicyRuleMaxTransactionAmount)
	}
}

func TestCheckTransactionPolicy_DailyVolumeLimit(t *testing.T) {
	policy := TenantPolicy{DailyVolumeLimit: 1000}

	// Already posted 900 today; this transaction adds 50 more (950 total): fine.
	underCap := []Posting{
		mustPosting(t, "a", 50, "USD"),
		mustPosting(t, "b", -50, "USD"),
	}
	if err := CheckTransactionPolicy(policy, underCap, map[string]int64{"USD": 900}); err != nil {
		t.Errorf("under daily cap rejected: %v", err)
	}

	// Already posted 900 today; this transaction adds 150 more (1050 total): over.
	overCap := []Posting{
		mustPosting(t, "a", 150, "USD"),
		mustPosting(t, "b", -150, "USD"),
	}
	err := CheckTransactionPolicy(policy, overCap, map[string]int64{"USD": 900})
	var pv *PolicyViolationError
	if !errors.As(err, &pv) {
		t.Fatalf("over daily cap: err = %v, want *PolicyViolationError", err)
	}
	if pv.Rule != PolicyRuleDailyVolumeLimit || pv.Currency != "USD" || pv.Amount != 1050 || pv.Limit != 1000 {
		t.Errorf("PolicyViolationError = %+v, want Rule=%s Currency=USD Amount=1050 Limit=1000", pv, PolicyRuleDailyVolumeLimit)
	}

	// A different currency, already posted 900 today in USD, is unaffected:
	// EUR has no daily total recorded yet, so 150 EUR is nowhere near 1000.
	eur := []Posting{
		mustPosting(t, "a", 150, "EUR"),
		mustPosting(t, "b", -150, "EUR"),
	}
	if err := CheckTransactionPolicy(policy, eur, map[string]int64{"USD": 900}); err != nil {
		t.Errorf("different currency affected by USD daily total: %v", err)
	}
}

func TestPolicyViolationError_Unwrap(t *testing.T) {
	err := &PolicyViolationError{Rule: PolicyRuleMaxTransactionAmount, Currency: "USD", Amount: 100, Limit: 50}
	if !errors.Is(err, ErrPolicyViolation) {
		t.Error("PolicyViolationError does not match ErrPolicyViolation via errors.Is")
	}
}
