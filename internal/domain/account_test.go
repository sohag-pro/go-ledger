package domain

import (
	"errors"
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
