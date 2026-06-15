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
