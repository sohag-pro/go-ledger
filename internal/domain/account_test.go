package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestAccountTypeValid(t *testing.T) {
	valid := []AccountType{Asset, Liability, Equity, Income, Expense}
	for _, at := range valid {
		if err := at.Validate(); err != nil {
			t.Errorf("Validate(%v) = %v, want nil", at, err)
		}
	}
	bad := []AccountType{AccountType(0), AccountType(99), AccountType(-1)}
	for _, at := range bad {
		if err := at.Validate(); !errors.Is(err, ErrInvalidAccountType) {
			t.Errorf("Validate(%d) = %v, want ErrInvalidAccountType", at, err)
		}
	}
}

func TestAccountTypeNormalBalance(t *testing.T) {
	// Assets and expenses increase on the debit side; liabilities, equity, and
	// income increase on the credit side.
	debitNormal := map[AccountType]bool{
		Asset: true, Expense: true,
		Liability: false, Equity: false, Income: false,
	}
	for at, wantDebit := range debitNormal {
		if got := at.IsDebitNormal(); got != wantDebit {
			t.Errorf("IsDebitNormal(%v) = %v, want %v", at, got, wantDebit)
		}
	}
}

func TestAccountTypeString(t *testing.T) {
	cases := map[AccountType]string{
		Asset: "asset", Liability: "liability", Equity: "equity",
		Income: "income", Expense: "expense", AccountType(0): "unknown",
	}
	for at, want := range cases {
		if got := at.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", at, got, want)
		}
	}
}

func TestParseAccountType(t *testing.T) {
	tests := []struct {
		name    string
		s       string
		want    AccountType
		wantErr error
	}{
		{"asset", "asset", Asset, nil},
		{"liability", "liability", Liability, nil},
		{"equity", "equity", Equity, nil},
		{"income", "income", Income, nil},
		{"expense", "expense", Expense, nil},
		{"unknown name", "bogus", 0, ErrInvalidAccountType},
		{"empty string", "", 0, ErrInvalidAccountType},
		{"wrong case", "Asset", 0, ErrInvalidAccountType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAccountType(tt.s)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseAccountType(%q) err = %v, want %v", tt.s, err, tt.wantErr)
			}
			if tt.wantErr == nil && got != tt.want {
				t.Errorf("ParseAccountType(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestAccountValidate(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		accName string
		typ     AccountType
		cur     Currency
		wantErr error
	}{
		{"valid", "acc_1", "Cash", Asset, "USD", nil},
		{"empty id", "", "Cash", Asset, "USD", ErrInvalidAccount},
		{"empty name", "acc_1", "", Asset, "USD", ErrInvalidAccount},
		{"bad type", "acc_1", "Cash", AccountType(0), "USD", ErrInvalidAccountType},
		{"bad currency", "acc_1", "Cash", Asset, "usd", ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := Account{ID: tt.id, Name: tt.accName, Type: tt.typ, Currency: tt.cur}
			if err := a.Validate(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestAccountStatusValid covers Task 5.5 (audit A1.5): the three defined
// statuses are valid, the zero value and any unrecognized string are not.
func TestAccountStatusValid(t *testing.T) {
	valid := []AccountStatus{AccountActive, AccountFrozen, AccountClosed}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	invalid := []AccountStatus{"", "bogus", "ACTIVE", "suspended"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
}

// TestAccountValidateStatus covers Account.Validate's Status handling (Task
// 5.5, audit A1.5): an empty Status is treated as "unset" (valid, not an
// error, see Account.Status's doc comment), a recognized Status is valid,
// and an unrecognized Status is ErrInvalidAccount.
func TestAccountValidateStatus(t *testing.T) {
	base := Account{ID: "acc_1", Name: "Cash", Type: Asset, Currency: "USD"}
	tests := []struct {
		name    string
		status  AccountStatus
		wantErr error
	}{
		{"unset", "", nil},
		{"active", AccountActive, nil},
		{"frozen", AccountFrozen, nil},
		{"closed", AccountClosed, nil},
		{"bogus", AccountStatus("bogus"), ErrInvalidAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := base
			a.Status = tt.status
			if err := a.Validate(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestAccountValidatePartyFields covers Task 6.1 (audit A9.1): PartyReference
// and PartyType are both optional (nil is valid, the default), any value up
// to their length cap is valid, and exceeding either cap is rejected with a
// distinct sentinel per field.
func TestAccountValidatePartyFields(t *testing.T) {
	base := Account{ID: "acc_1", Name: "Cash", Type: Asset, Currency: "USD"}
	ptr := func(s string) *string { return &s }
	atCap := strings.Repeat("a", MaxPartyReferenceLen)
	overCap := strings.Repeat("a", MaxPartyReferenceLen+1)

	tests := []struct {
		name           string
		partyReference *string
		partyType      *string
		wantErr        error
	}{
		{"both unset", nil, nil, nil},
		{"party reference set, no type", ptr("cust-123"), nil, nil},
		{"both set", ptr("cust-123"), ptr("individual"), nil},
		{"party reference at cap", ptr(atCap), nil, nil},
		{"party reference over cap", ptr(overCap), nil, ErrPartyReferenceTooLong},
		{"party type at cap", nil, ptr(atCap), nil},
		{"party type over cap", nil, ptr(overCap), ErrPartyTypeTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := base
			a.PartyReference = tt.partyReference
			a.PartyType = tt.partyType
			if err := a.Validate(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// mustPosting is defined in tenant_test.go and reused here.

// TestCheckAccountPostingConstraints covers Task 5.5 (audit A1.5)'s pure
// domain check in isolation, independent of any storage adapter: frozen and
// closed non-system accounts are rejected, active ones pass; a min_balance
// breach is rejected, staying at or above it passes, and an account with no
// min_balance is unconstrained; a system account is exempt from both checks
// even when frozen or breaching what would otherwise be its floor; and an
// account id in postings with no matching states entry is ErrAccountNotFound.
func TestCheckAccountPostingConstraints(t *testing.T) {
	floor := int64(-1000)
	tests := []struct {
		name     string
		states   map[string]AccountPostingState
		postings []Posting
		wantErr  error
	}{
		{
			name: "active posts",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountActive},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: nil,
		},
		{
			name: "unset status treated as active",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: ""},
				"b": {AccountID: "b", Status: ""},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: nil,
		},
		{
			name: "frozen rejected",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountFrozen},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: ErrAccountNotActive,
		},
		{
			name: "closed rejected",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountClosed},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: ErrAccountNotActive,
		},
		{
			name: "min balance breach rejected",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountActive, MinBalance: &floor, Balance: -600},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				// -600 (current) + -500 (this posting) = -1100, below -1000.
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: ErrMinBalanceBreach,
		},
		{
			name: "min balance held exactly passes",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountActive, MinBalance: &floor, Balance: -600},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				// -600 + -400 = -1000, exactly at the floor.
				mustPosting(t, "a", -400, "USD"),
				mustPosting(t, "b", 400, "USD"),
			},
			wantErr: nil,
		},
		{
			name: "no min balance unconstrained",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountActive, Balance: -1_000_000},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: nil,
		},
		{
			name: "system account exempt from frozen status",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountFrozen, IsSystem: true},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: nil,
		},
		{
			name: "system account exempt from min balance",
			states: map[string]AccountPostingState{
				"a": {AccountID: "a", Status: AccountActive, IsSystem: true, MinBalance: &floor, Balance: -1_000_000},
				"b": {AccountID: "b", Status: AccountActive},
			},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: nil,
		},
		{
			name:   "missing state entry is account not found",
			states: map[string]AccountPostingState{"b": {AccountID: "b", Status: AccountActive}},
			postings: []Posting{
				mustPosting(t, "a", -500, "USD"),
				mustPosting(t, "b", 500, "USD"),
			},
			wantErr: ErrAccountNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckAccountPostingConstraints(tt.states, tt.postings)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckAccountPostingConstraints() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestAccountNotActiveErrorMessage covers the caller-facing message names
// the account and its exact status (Task 5.5, audit A1.5).
func TestAccountNotActiveErrorMessage(t *testing.T) {
	err := &AccountNotActiveError{AccountID: "acc_1", Status: AccountFrozen}
	if !errors.Is(err, ErrAccountNotActive) {
		t.Errorf("errors.Is(err, ErrAccountNotActive) = false, want true")
	}
	const want = "domain: account acc_1 is frozen"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestMinBalanceBreachErrorMessage covers the caller-facing message names
// the account, the balance the posting would have produced, and the floor
// it breaches (Task 5.5, audit A1.5).
func TestMinBalanceBreachErrorMessage(t *testing.T) {
	err := &MinBalanceBreachError{AccountID: "acc_1", MinBalance: -1000, NewBalance: -1100}
	if !errors.Is(err, ErrMinBalanceBreach) {
		t.Errorf("errors.Is(err, ErrMinBalanceBreach) = false, want true")
	}
	const want = "domain: posting would take account acc_1 to -1100, below its minimum balance -1000"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
