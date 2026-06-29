// Package seed populates the demo ledger with realistic, backdated data. It is a
// demo tool, not part of the core service: it writes rows directly (with explicit
// created_at, which the service does not allow) so a statement reads like a real
// history. The balance and currency triggers still validate every transaction, so
// seeded data is provably correct.
package seed

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	spendingTxns = 95 // postings on Spending and Checking
	incomeTxns   = 95 // postings on Income and Checking
	savingsTxns  = 95 // postings on Savings and Checking
	historyDays  = 90 // transactions spread across this many past days
)

// category is a labeled amount range in minor units (cents).
type category struct {
	desc     string
	min, max int64
}

// Weighted by repetition: common, small items appear more often than monthly bills.
var (
	spendingCats = []category{
		{"Groceries", 2500, 9500},
		{"Groceries", 2500, 9500},
		{"Groceries", 2500, 9500},
		{"Coffee", 350, 750},
		{"Coffee", 350, 750},
		{"Coffee", 350, 750},
		{"Coffee", 350, 750},
		{"Dining out", 1800, 7500},
		{"Dining out", 1800, 7500},
		{"Transport", 200, 1800},
		{"Transport", 200, 1800},
		{"Transport", 200, 1800},
		{"Pharmacy", 800, 4000},
		{"Clothing", 2500, 12000},
		{"Streaming subscription", 999, 1999},
		{"Electricity bill", 3500, 9000},
		{"Internet bill", 4000, 6000},
		{"Phone bill", 2000, 4500},
		{"Gym membership", 3000, 5000},
		{"Rent", 110000, 140000},
	}
	incomeCats = []category{
		{"Freelance project", 50000, 200000},
		{"Interest", 200, 2000},
		{"Interest", 200, 2000},
		{"Cashback reward", 100, 1500},
		{"Cashback reward", 100, 1500},
		{"Dividend", 1500, 12000},
		{"Tax refund", 80000, 250000},
		{"Gift received", 5000, 30000},
		{"Monthly salary", 320000, 380000},
		{"Monthly salary", 320000, 380000},
	}
	savingsCats = []category{
		{"Auto-save round-up", 100, 2000},
		{"Auto-save round-up", 100, 2000},
		{"Auto-save round-up", 100, 2000},
		{"Monthly savings transfer", 20000, 60000},
		{"Goal contribution", 5000, 25000},
	}
)

// posting is one leg of a seeded transaction.
type posting struct {
	accountID string
	amount    int64
	desc      string
}

// txn is a balanced, timestamped transaction to insert.
type txn struct {
	at   time.Time
	legs [2]posting
}

// Seed resets the tenant's ledger and repopulates it with four personal-finance
// accounts and a few hundred backdated transactions. It is atomic: everything
// happens in one database transaction, so the API never observes a half-seeded
// ledger. now is the reference time (the most recent possible transaction).
func Seed(ctx context.Context, pool *pgxpool.Pool, tenantID string, now time.Time) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("seed: parse tenant id: %w", err)
	}
	rng := rand.New(rand.NewSource(now.UnixNano())) //nolint:gosec // demo data, not crypto

	checking, savings, income, spending := newID(), newID(), newID(), newID()
	accountsAt := now.AddDate(0, 0, -(historyDays + 1))

	// Build the transactions. Signed double-entry: positive debit, negative credit.
	var txns []txn
	for i := 0; i < spendingTxns; i++ {
		c := spendingCats[rng.Intn(len(spendingCats))]
		amt := c.min + rng.Int63n(c.max-c.min+1)
		// spend: Spending up (debit), Checking down (credit)
		txns = append(txns, txn{randTime(rng, now), [2]posting{
			{spending, amt, c.desc}, {checking, -amt, c.desc},
		}})
	}
	for i := 0; i < incomeTxns; i++ {
		c := incomeCats[rng.Intn(len(incomeCats))]
		amt := c.min + rng.Int63n(c.max-c.min+1)
		// earn: Checking up (debit), Income up as credit (negative)
		txns = append(txns, txn{randTime(rng, now), [2]posting{
			{checking, amt, c.desc}, {income, -amt, c.desc},
		}})
	}
	for i := 0; i < savingsTxns; i++ {
		c := savingsCats[rng.Intn(len(savingsCats))]
		amt := c.min + rng.Int63n(c.max-c.min+1)
		// save: Savings up (debit), Checking down (credit)
		txns = append(txns, txn{randTime(rng, now), [2]posting{
			{savings, amt, c.desc}, {checking, -amt, c.desc},
		}})
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("seed: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit

	// Reset: postings reference transactions and accounts, so delete them first.
	for _, table := range []string{"postings", "transactions", "accounts"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tid); err != nil {
			return fmt.Errorf("seed: clear %s: %w", table, err)
		}
	}

	accounts := []struct {
		id, name, typ string
	}{
		{checking, "Checking", "asset"},
		{savings, "Savings", "asset"},
		{income, "Income", "income"},
		{spending, "Spending", "expense"},
	}
	for _, a := range accounts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO accounts (id, tenant_id, name, type, currency, created_at) VALUES ($1,$2,$3,$4,'USD',$5)`,
			a.id, tid, a.name, a.typ, accountsAt); err != nil {
			return fmt.Errorf("seed: insert account %s: %w", a.name, err)
		}
	}

	for _, t := range txns {
		txID := newID()
		if _, err := tx.Exec(ctx,
			`INSERT INTO transactions (id, tenant_id, currency, created_at) VALUES ($1,$2,'USD',$3)`,
			txID, tid, t.at); err != nil {
			return fmt.Errorf("seed: insert transaction: %w", err)
		}
		for _, leg := range t.legs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, description, created_at)
				 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				newID(), tid, txID, leg.accountID, leg.amount, leg.desc, t.at); err != nil {
				return fmt.Errorf("seed: insert posting: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("seed: commit: %w", err)
	}
	return nil
}

// newID returns a fresh UUIDv7 string. The id encodes generation time, but
// created_at (backdated) is the source of truth for ordering.
func newID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// randTime returns a random instant within the last historyDays, to second
// precision, so seeded transactions read like a real spread of activity.
func randTime(rng *rand.Rand, now time.Time) time.Time {
	secs := rng.Int63n(historyDays * 24 * 3600)
	return now.Add(-time.Duration(secs) * time.Second)
}
