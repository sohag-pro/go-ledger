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
// currency is the ISO 4217 code stamped on every seeded account and posting
// (ADR-014, "New-account default currency is env-configured"); an empty
// currency falls back to "USD" so a direct caller that does not care about
// multi-currency still gets a valid, single-currency demo ledger.
//
// demoKeyHash is the SHA-256 hash (domain.HashAPIKey) of the demo api key.
// Before any destructive reset, Seed checks whether tenantID holds an api key
// other than that one; if it does, Seed refuses and wipes nothing (see
// ADR-015, "Safe-by-default deployment"). This is the guard against the
// seeder ever destroying a real tenant's data if DEFAULT_TENANT_ID is ever
// misconfigured to point at a live tenant instead of the demo one.
//
// The reset also clears api-sourced FX config that would otherwise survive:
// global rows (fx_rates and fx_markup_defaults with tenant_id NULL, source
// 'api') plus the demo tenant's OWN api-sourced fx_rates (fx_rates is not in
// the tenant-scoped delete loop below, and CurrentFXRate prefers a
// tenant-owned row over the global one, so a tenant-scoped tampered rate would
// otherwise persist and mis-price this tenant's conversions every reset).
// Env-seeded global rows (source 'env', re-asserted at boot by fx.Seed) are
// left alone.
func Seed(ctx context.Context, pool *pgxpool.Pool, tenantID string, now time.Time, currency, demoKeyHash string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return fmt.Errorf("seed: parse tenant id: %w", err)
	}
	if currency == "" {
		currency = "USD"
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

	// Ensure the tenant row exists before writing any tenant-owned data:
	// accounts_tenant_fk and transactions_tenant_fk (migration 0011, Task 2.1)
	// require it. ON CONFLICT DO NOTHING: a tenant already provisioned via
	// cmd/server's provisionAPIKeys (which runs before the seeder starts) is
	// left exactly as it is, including any name or status an operator may
	// already have set; only a tenant that has never been provisioned gets a
	// placeholder row here.
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		tid, "demo-"+tenantID[:8]); err != nil {
		return fmt.Errorf("seed: ensure tenant row: %w", err)
	}

	// Refuse to touch a tenant that holds any api key other than the demo key,
	// before any destructive statement runs. A misconfigured DEFAULT_TENANT_ID
	// pointed at a live tenant must never lose data to a periodic demo reset.
	var foreignKeys int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM api_keys WHERE tenant_id = $1 AND key_hash != $2",
		tid, demoKeyHash).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("seed: check api keys for tenant %s: %w", tenantID, err)
	}
	if foreignKeys > 0 {
		return fmt.Errorf("seed: refusing to reset tenant %s: it holds %d api key(s) other than the demo key", tenantID, foreignKeys)
	}

	// audit_log is append-only, guarded by a trigger that rejects UPDATE/DELETE.
	// The demo seeder is the one sanctioned exception: this transaction-local GUC
	// lets the reset clear audit rows. Only the seeder sets it; the service path
	// never does, so the log stays immutable in normal operation.
	if _, err := tx.Exec(ctx, "SET LOCAL audit.allow_purge = 'on'"); err != nil {
		return fmt.Errorf("seed: enable audit purge: %w", err)
	}

	// Reset: idempotency_keys, audit_log, and audit_outbox (ADR-017) all
	// reference transactions, so clear them first, then postings and
	// transactions before accounts. audit_outbox is not append-only guarded
	// (no immutability trigger; it holds no chain data, just pending
	// events), so it needs no purge GUC, unlike audit_log above.
	for _, table := range []string{"idempotency_keys", "audit_log", "audit_outbox", "postings", "transactions", "accounts"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tid); err != nil {
			return fmt.Errorf("seed: clear %s: %w", table, err)
		}
	}

	// The public demo exposes the FX admin endpoints (internal/api/fx_admin.go)
	// with no auth in demo mode, so any anonymous visitor can POST a global
	// markup or a garbage global mid rate through them. Those rows are global
	// (tenant_id NULL), not scoped to the demo tenant, so the tenant-scoped
	// loop above never touches them and, unlike the tenant's own data, they
	// would otherwise survive every reset and mis-price every other visitor's
	// conversions. Clear only source='api' rows (the admin-API write path,
	// internal/fx/admin.go's apiSource): source='env' rows are re-asserted at
	// every boot from FX_RATES by fx.Seed, so they are left alone and the
	// demo's configured rates still apply right after a reset. fx_rates is
	// NOT in the tenant-scoped delete loop above, so a tenant-scoped api row
	// (an anonymous visitor can POST /v1/admin/fx/rates with tenant_id set to
	// the demo tenant) is not covered by that loop either: CurrentFXRate
	// prefers a tenant-owned row over the global one, so a garbage tenant
	// rate would otherwise survive every reset and mis-price every demo
	// conversion. Clear both the global and the demo tenant's own api-sourced
	// rows here.
	if _, err := tx.Exec(ctx, "DELETE FROM fx_rates WHERE tenant_id IS NULL AND source = 'api'"); err != nil {
		return fmt.Errorf("seed: clear api-sourced global fx rates: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM fx_rates WHERE tenant_id = $1 AND source = 'api'", tid); err != nil {
		return fmt.Errorf("seed: clear api-sourced demo tenant fx rates: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM fx_markup_defaults WHERE source = 'api' AND (tenant_id IS NULL OR tenant_id = $1)", tid); err != nil {
		return fmt.Errorf("seed: clear api-sourced fx markup defaults: %w", err)
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
			`INSERT INTO accounts (id, tenant_id, name, type, currency, created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			a.id, tid, a.name, a.typ, currency, accountsAt); err != nil {
			return fmt.Errorf("seed: insert account %s: %w", a.name, err)
		}
	}

	// transactions no longer carries a currency column (ADR-014, migration
	// 0010): currency lives on each posting instead, since an FX transaction
	// spans two currencies. Every seeded leg below stamps the same currency,
	// since the demo ledger is single-currency by construction.
	for _, t := range txns {
		txID := newID()
		if _, err := tx.Exec(ctx,
			`INSERT INTO transactions (id, tenant_id, created_at) VALUES ($1,$2,$3)`,
			txID, tid, t.at); err != nil {
			return fmt.Errorf("seed: insert transaction: %w", err)
		}
		for _, leg := range t.legs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO postings (id, tenant_id, transaction_id, account_id, amount, description, currency, created_at)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				newID(), tid, txID, leg.accountID, leg.amount, leg.desc, currency, t.at); err != nil {
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
