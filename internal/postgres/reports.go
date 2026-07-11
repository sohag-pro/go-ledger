package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres/sqlc"
)

// TrialBalanceByCurrency returns tenantID's net posted total per currency
// (Task 6.3, audit A9.2): the double-entry balance proof. Runs under
// withTenant, so it inherits the RLS defense in depth every other
// tenant-scoped read here does.
func (r *Repository) TrialBalanceByCurrency(ctx context.Context, tenantID string) ([]domain.CurrencyTotal, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var rows []sqlc.TrialBalanceByCurrencyRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.TrialBalanceByCurrency(ctx, tid)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: trial balance by currency: %w", err)
	}
	out := make([]domain.CurrencyTotal, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.CurrencyTotal{Currency: domain.Currency(row.Currency), Net: row.Net})
	}
	return out, nil
}

// TrialBalanceAccounts returns every one of tenantID's accounts with its
// derived balance, including system (FX clearing) accounts (Task 6.3, audit
// A9.2).
func (r *Repository) TrialBalanceAccounts(ctx context.Context, tenantID string) ([]domain.AccountBalance, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse tenant id: %w", err)
	}
	var rows []sqlc.TrialBalanceAccountsRow
	err = r.withTenant(ctx, tenantID, func(q *sqlc.Queries) error {
		var err error
		rows, err = q.TrialBalanceAccounts(ctx, tid)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: trial balance accounts: %w", err)
	}
	out := make([]domain.AccountBalance, 0, len(rows))
	for _, row := range rows {
		accountType, err := domain.ParseAccountType(row.Type)
		if err != nil {
			return nil, fmt.Errorf("postgres: trial balance account type: %w", err)
		}
		out = append(out, domain.AccountBalance{
			AccountID: row.ID.String(),
			Name:      row.Name,
			Type:      accountType,
			Currency:  domain.Currency(row.Currency),
			IsSystem:  row.IsSystem,
			Balance:   row.Balance,
		})
	}
	return out, nil
}
