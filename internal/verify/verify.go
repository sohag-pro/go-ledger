// Package verify checks a restored ledger database against the ledger's own
// invariants (ADR-016, "Automated restore-and-verify"). A backup is not
// trusted until a restore is proven, so this package is run against a
// throwaway database that was just restored from the offsite backup, never
// against the live production database.
//
// It checks two things:
//
//  1. The core double-entry invariant (ADR-001, enforced per currency since
//     ADR-014): every transaction's postings sum to zero, per currency.
//  2. Every tenant's tamper-evident audit hash chain (ADR-012) still verifies
//     end to end, reusing ledger.AuditService.Verify rather than
//     reimplementing the hashing.
//
// Both checks are read-only queries against the restored database; nothing
// here writes.
package verify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/ledger"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// BalanceViolation is one transaction, currency pair whose postings do not
// sum to zero: a direct violation of the core double-entry invariant.
type BalanceViolation struct {
	TransactionID string
	Currency      string
	Sum           int64
}

// ChainBreak is one tenant whose audit hash chain failed to verify.
// FirstBreakID is the id of the first audit row that failed to recompute
// (see ledger.AuditService.Verify), and Checked is how many rows were walked
// before the break was found.
type ChainBreak struct {
	TenantID     string
	FirstBreakID string
	Checked      int
}

// Report is the outcome of verifying a restored ledger database.
type Report struct {
	BalanceViolations []BalanceViolation
	TenantsChecked    int
	ChainBreaks       []ChainBreak
}

// OK reports whether the restored database is free of both balance
// violations and audit chain breaks.
func (r Report) OK() bool {
	return len(r.BalanceViolations) == 0 && len(r.ChainBreaks) == 0
}

// Run verifies the ledger invariants against the database behind pool. It
// returns an error only for infrastructure failures (a query or connection
// failure); a ledger that fails its own invariants is a successful run that
// returns a non-nil error, only a Report whose OK() is false.
func Run(ctx context.Context, pool *pgxpool.Pool) (Report, error) {
	var report Report

	violations, err := balanceViolations(ctx, pool)
	if err != nil {
		return Report{}, fmt.Errorf("verify: balance invariant: %w", err)
	}
	report.BalanceViolations = violations

	breaks, checked, err := chainBreaks(ctx, pool)
	if err != nil {
		return Report{}, fmt.Errorf("verify: audit hash chain: %w", err)
	}
	report.ChainBreaks = breaks
	report.TenantsChecked = checked

	return report, nil
}

// balanceViolations runs the core zero-sum check: every transaction's
// postings must sum to zero per currency (mirroring the assert_txn_balanced
// trigger's rule; see internal/postgres/migrations/0010_multi_currency_fx.sql).
// A healthy ledger returns no rows.
func balanceViolations(ctx context.Context, pool *pgxpool.Pool) ([]BalanceViolation, error) {
	rows, err := pool.Query(ctx, `
		SELECT transaction_id, currency, SUM(amount)
		FROM postings
		GROUP BY transaction_id, currency
		HAVING SUM(amount) <> 0
	`)
	if err != nil {
		return nil, fmt.Errorf("query posting sums: %w", err)
	}
	defer rows.Close()

	var violations []BalanceViolation
	for rows.Next() {
		var v BalanceViolation
		if err := rows.Scan(&v.TransactionID, &v.Currency, &v.Sum); err != nil {
			return nil, fmt.Errorf("scan posting sum: %w", err)
		}
		violations = append(violations, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posting sums: %w", err)
	}
	return violations, nil
}

// chainBreaks discovers every tenant that has audit rows and walks each
// tenant's hash chain via the real production verifier (ledger.AuditService,
// backed by postgres.Repository), so this package never reimplements the
// hashing. It returns the tenants whose chain failed and the total count of
// tenants checked (whether they passed or failed).
func chainBreaks(ctx context.Context, pool *pgxpool.Pool) ([]ChainBreak, int, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT tenant_id FROM audit_log ORDER BY tenant_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("query distinct tenants: %w", err)
	}
	var tenantIDs []string
	for rows.Next() {
		var tenantID string
		if err := rows.Scan(&tenantID); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan tenant id: %w", err)
		}
		tenantIDs = append(tenantIDs, tenantID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("iterate distinct tenants: %w", err)
	}
	rows.Close()

	repo := postgres.NewRepository(pool)
	audit := ledger.NewAuditService(repo)

	var breaks []ChainBreak
	for _, tenantID := range tenantIDs {
		result, err := audit.Verify(ctx, tenantID)
		if err != nil {
			return nil, 0, fmt.Errorf("verify audit chain for tenant %s: %w", tenantID, err)
		}
		if !result.Valid {
			breaks = append(breaks, ChainBreak{
				TenantID:     tenantID,
				FirstBreakID: result.FirstBreakID,
				Checked:      result.Checked,
			})
		}
	}
	return breaks, len(tenantIDs), nil
}
