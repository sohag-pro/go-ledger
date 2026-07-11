package postgres_test

// Task 6.3 / audit A9.2: internal/postgres/reports.go's two trial-balance
// queries, driven directly against a real Postgres.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// TestReportsRepo_TrialBalance covers both TrialBalanceByCurrency and
// TrialBalanceAccounts: after a balanced two-leg transaction, the currency
// total nets to zero (the double-entry balance proof, ADR-001) and each
// account's own balance is reported correctly, including a system (FX
// clearing) account.
func TestReportsRepo_TrialBalance(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "reports repo test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	cash := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	revenue := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, cash); err != nil {
		t.Fatalf("create cash: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, revenue); err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	// A system (FX clearing) account with no postings: TrialBalanceAccounts
	// must include it, marked IsSystem, at a zero balance.
	clearing, err := repo.GetOrCreateClearingAccount(ctx, tenant, "USD")
	if err != nil {
		t.Fatalf("get or create clearing account: %v", err)
	}

	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: cash.ID, Amount: money(t, 7500, "USD")},
		{AccountID: revenue.ID, Amount: money(t, -7500, "USD")},
	}}
	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	totals, err := repo.TrialBalanceByCurrency(ctx, tenant)
	if err != nil {
		t.Fatalf("trial balance by currency: %v", err)
	}
	if len(totals) != 1 {
		t.Fatalf("trial balance by currency = %d rows, want 1 (USD only)", len(totals))
	}
	if totals[0].Currency != "USD" {
		t.Errorf("currency = %q, want USD", totals[0].Currency)
	}
	if totals[0].Net != 0 {
		t.Errorf("net = %d, want 0 (the double-entry balance proof)", totals[0].Net)
	}

	accounts, err := repo.TrialBalanceAccounts(ctx, tenant)
	if err != nil {
		t.Fatalf("trial balance accounts: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("trial balance accounts = %d rows, want 3 (cash, revenue, clearing)", len(accounts))
	}
	byID := make(map[string]domain.AccountBalance, len(accounts))
	for _, a := range accounts {
		byID[a.AccountID] = a
	}
	cashRow, ok := byID[cash.ID]
	if !ok {
		t.Fatal("trial balance accounts: missing cash account")
	}
	if cashRow.Balance != 7500 {
		t.Errorf("cash balance = %d, want 7500", cashRow.Balance)
	}
	if cashRow.Type != domain.Asset {
		t.Errorf("cash type = %q, want %q", cashRow.Type, domain.Asset)
	}
	if cashRow.IsSystem {
		t.Error("cash account reported IsSystem = true, want false")
	}
	clearingRow, ok := byID[clearing.ID]
	if !ok {
		t.Fatal("trial balance accounts: missing clearing account")
	}
	if !clearingRow.IsSystem {
		t.Error("clearing account reported IsSystem = false, want true")
	}
	if clearingRow.Balance != 0 {
		t.Errorf("clearing balance = %d, want 0 (no postings)", clearingRow.Balance)
	}
}

// TestReportsRepo_EmptyTenant proves both trial-balance queries return an
// empty (not nil-panicking, not erroring) slice for a tenant with no accounts
// or postings at all.
func TestReportsRepo_EmptyTenant(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	if err := repo.CreateTenant(ctx, tenant, "reports repo empty test tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	totals, err := repo.TrialBalanceByCurrency(ctx, tenant)
	if err != nil {
		t.Fatalf("trial balance by currency: %v", err)
	}
	if len(totals) != 0 {
		t.Errorf("trial balance by currency for an empty tenant = %d rows, want 0", len(totals))
	}

	accounts, err := repo.TrialBalanceAccounts(ctx, tenant)
	if err != nil {
		t.Fatalf("trial balance accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("trial balance accounts for an empty tenant = %d rows, want 0", len(accounts))
	}
}

// TestReportsRepo_MalformedTenantID proves both queries fail closed with a
// parse error for a syntactically invalid tenant id.
func TestReportsRepo_MalformedTenantID(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	const bad = "not-a-uuid"

	if _, err := repo.TrialBalanceByCurrency(ctx, bad); err == nil {
		t.Error("TrialBalanceByCurrency(bad tenant): expected a parse error, got nil")
	}
	if _, err := repo.TrialBalanceAccounts(ctx, bad); err == nil {
		t.Error("TrialBalanceAccounts(bad tenant): expected a parse error, got nil")
	}
}
