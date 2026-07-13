// Package seed populates the demo ledger with realistic, backdated data. It is a
// demo tool, not part of the core service: it writes rows directly (with explicit
// created_at, which the service does not allow) so a statement reads like a real
// history. The balance and currency triggers still validate every transaction, so
// seeded data is provably correct.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/fx"
)

// The demo runs three tenants, each a different persona so a visitor can relate:
// a personal budget (the default tenant), a bank's own books, and a company's
// books. The two non-default ids are fixed and high enough to stay clear of the
// demo tenant (...001) and the load-test tenants (offset 200, see cmd/server).
const (
	bankTenantID    = "00000000-0000-0000-0000-000000000011"
	companyTenantID = "00000000-0000-0000-0000-000000000012"
)

// historyDays is how far back seeded transactions are spread.
const historyDays = 90

// category is a labeled amount range in minor units (cents).
type category struct {
	desc     string
	min, max int64
}

// acct is one account in a theme's chart of accounts. cur empty means the
// theme's own currency; a non-empty cur (the foreign accounts) overrides it.
type acct struct {
	name, typ, cur string
}

// flow is a repeated double-entry pattern: count transactions, each debiting
// the debit account and crediting the credit account by an amount drawn from
// cats. Debit is the positive leg, credit the negative, so every generated
// transaction sums to zero in the theme currency.
type flow struct {
	debit, credit string
	count         int
	cats          []category
}

// theme is one demo tenant: its id, display name, home currency, chart of
// accounts, and the flows that generate its backdated history.
type theme struct {
	id, name, currency string
	accounts           []acct
	flows              []flow
}

// foreignAccounts are the blank multi-currency accounts every demo tenant
// holds, so there is somewhere to convert into out of the box (the USD-hub
// currencies, ADR-022). They carry no flows, so they start at a zero balance.
var foreignAccounts = []acct{
	{"Euro Account", "asset", "EUR"},
	{"Taka Account", "asset", "BDT"},
	{"Ringgit Account", "asset", "MYR"},
}

// personalTheme is a person's everyday budget: salary in, rent and living costs
// out, some saved and invested, a credit card paid down.
func personalTheme(id, currency string) theme {
	return theme{
		id: id, name: "Ava Thompson", currency: currency,
		accounts: []acct{
			{"Checking", "asset", ""},
			{"Savings", "asset", ""},
			{"Investments", "asset", ""},
			{"Credit Card", "liability", ""},
			{"Salary", "income", ""},
			{"Other Income", "income", ""},
			{"Housing", "expense", ""},
			{"Living Expenses", "expense", ""},
		},
		flows: []flow{
			{"Checking", "Salary", 6, []category{{"Monthly salary", 320000, 380000}}},
			{"Checking", "Other Income", 22, []category{
				{"Interest", 200, 2000}, {"Cashback reward", 100, 1500}, {"Dividend", 1500, 12000},
			}},
			{"Housing", "Checking", 3, []category{{"Rent", 110000, 140000}}},
			{"Living Expenses", "Checking", 55, []category{
				{"Groceries", 2500, 9500},
				{"Coffee", 350, 750},
				{"Dining out", 1800, 7500},
				{"Transport", 200, 1800},
				{"Pharmacy", 800, 4000},
				{"Electricity bill", 3500, 9000},
				{"Internet bill", 4000, 6000},
				{"Phone bill", 2000, 4500},
			}},
			{"Living Expenses", "Credit Card", 25, []category{
				{"Online order", 1500, 20000},
				{"Clothing", 2500, 12000},
				{"Dining out", 1800, 7500},
				{"Streaming subscription", 999, 1999},
			}},
			{"Credit Card", "Checking", 3, []category{{"Credit card payment", 50000, 150000}}},
			{"Savings", "Checking", 6, []category{{"Monthly savings transfer", 20000, 60000}}},
			{"Investments", "Checking", 4, []category{{"Investment contribution", 30000, 100000}}},
		},
	}
}

// bankTheme is a bank's own general ledger: customer deposits and withdrawals,
// loans out and repaid, interest and fee income, operating costs, and capital.
func bankTheme(id string) theme {
	return theme{
		id: id, name: "Harbor National Bank", currency: "USD",
		accounts: []acct{
			{"Cash Reserves", "asset", ""},
			{"Loans Receivable", "asset", ""},
			{"Customer Deposits", "liability", ""},
			{"Interest Income", "income", ""},
			{"Fee Income", "income", ""},
			{"Operating Expenses", "expense", ""},
			{"Share Capital", "equity", ""},
		},
		flows: []flow{
			{"Cash Reserves", "Customer Deposits", 45, []category{{"Customer deposit", 50000, 5000000}}},
			{"Customer Deposits", "Cash Reserves", 30, []category{{"Customer withdrawal", 20000, 2000000}}},
			{"Loans Receivable", "Cash Reserves", 15, []category{{"Loan disbursement", 500000, 20000000}}},
			{"Cash Reserves", "Loans Receivable", 22, []category{{"Loan repayment", 50000, 1000000}}},
			{"Cash Reserves", "Interest Income", 25, []category{{"Loan interest", 10000, 300000}}},
			{"Cash Reserves", "Fee Income", 30, []category{
				{"Account fee", 500, 5000}, {"Wire transfer fee", 1500, 4000}, {"Overdraft fee", 2500, 6000},
			}},
			{"Operating Expenses", "Cash Reserves", 20, []category{
				{"Staff salaries", 200000, 800000}, {"Branch rent", 150000, 400000}, {"Utilities", 20000, 80000},
			}},
			{"Cash Reserves", "Share Capital", 2, []category{{"Capital contribution", 5000000, 20000000}}},
		},
	}
}

// companyTheme is a trading company's books: invoices raised and collected,
// stock bought on credit and paid for, payroll, rent, and owner capital.
func companyTheme(id string) theme {
	return theme{
		id: id, name: "Brightpeak Trading Ltd", currency: "USD",
		accounts: []acct{
			{"Cash", "asset", ""},
			{"Accounts Receivable", "asset", ""},
			{"Accounts Payable", "liability", ""},
			{"Sales Revenue", "income", ""},
			{"Cost of Goods Sold", "expense", ""},
			{"Payroll Expense", "expense", ""},
			{"Rent Expense", "expense", ""},
			{"Owner Equity", "equity", ""},
		},
		flows: []flow{
			{"Accounts Receivable", "Sales Revenue", 40, []category{{"Invoice raised", 50000, 800000}}},
			{"Cash", "Accounts Receivable", 35, []category{{"Invoice paid", 50000, 800000}}},
			{"Cost of Goods Sold", "Accounts Payable", 30, []category{{"Inventory purchase", 30000, 400000}}},
			{"Accounts Payable", "Cash", 25, []category{{"Supplier payment", 30000, 400000}}},
			{"Payroll Expense", "Cash", 6, []category{{"Monthly payroll", 200000, 600000}}},
			{"Rent Expense", "Cash", 3, []category{{"Office rent", 80000, 150000}}},
			{"Cash", "Owner Equity", 2, []category{{"Owner capital", 1000000, 5000000}}},
		},
	}
}

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

// Seed resets one tenant with the personal-finance theme and repopulates it
// with a realistic, backdated history. It is atomic: everything happens in one
// database transaction, so the API never observes a half-seeded ledger. Kept
// for direct callers and tests; the server uses Demo to seed all three demo
// tenants. now is the reference time (the most recent possible transaction).
// currency is the ISO 4217 code stamped on the personal accounts and their
// postings; an empty currency falls back to "USD".
//
// demoKeyHash is the SHA-256 hash (domain.HashAPIKey) of the demo api key.
// Before any destructive reset, Seed checks whether the tenant holds an api key
// other than that one; if it does, Seed refuses and wipes nothing (ADR-015,
// "Safe-by-default deployment"), the guard against ever destroying a real
// tenant's data if a tenant id is misconfigured to point at a live tenant.
func Seed(ctx context.Context, pool *pgxpool.Pool, tenantID string, now time.Time, currency, demoKeyHash string) error {
	if currency == "" {
		currency = "USD"
	}
	return seedTenant(ctx, pool, personalTheme(tenantID, currency), now, demoKeyHash)
}

// DemoTenantIDs returns the fixed ids of the three demo tenants (the personal
// budget on defaultTenantID, plus the bank and company on their own fixed ids).
// It is the "keep" set for PurgeNonDemoTenants: everything else is a
// visitor-created tenant the demo reset should wipe.
func DemoTenantIDs(defaultTenantID string) []string {
	return []string{defaultTenantID, bankTenantID, companyTenantID}
}

// purgeOrder lists the tenant-scoped tables to clear when deleting a tenant
// wholesale, in an order that never violates a foreign key: rows that reference
// another table come before the table they reference. postings, disputes,
// audit_log, audit_outbox, and idempotency_keys all reference transactions, so
// they go first; webhook_deliveries references webhook_subscriptions; the tenant
// row in tenants is deleted last, by PurgeNonDemoTenants itself. fx_rates and
// fx_markup_defaults are handled separately because their tenant_id is nullable
// (global rows must survive), so they are not in this list. pending_transactions
// (migration 0035, ADR-025) only references tenants, not transactions or
// accounts, so its place among the tenant-scoped tables is not FK-forced; it
// goes early anyway, alongside the other tables ahead of transactions, so a
// visitor tenant seeded with a pending (Task 12) never trips the final DELETE
// FROM tenants on a stray foreign key.
var purgeOrder = []string{
	"postings",
	"disputes",
	"audit_log",
	"audit_outbox",
	"idempotency_keys",
	"pending_transactions",
	"transactions",
	"accounts",
	"webhook_deliveries",
	"webhook_subscriptions",
	"api_keys",
	"crypto_keys",
	"audit_anchors",
}

// PurgeNonDemoTenants deletes every tenant whose id is not in keep, along with
// all of that tenant's data across every tenant-scoped table, and returns how
// many tenant rows it removed. It keeps the public demo clean: visitors can
// create tenants through the (demo-mode) admin panel, and without this they
// accumulate forever, since the seeder only ever resets the fixed demo ids.
//
// This is a DEMO-ONLY, deliberately destructive operation. Unlike seedTenant it
// does NOT honor the ADR-015 api-key guard: visitor tenants hold their own api
// keys, and the whole point here is to remove them. The only thing standing
// between this and a real deployment's data is the call site: it runs solely
// from runSeeder, which starts only when DEMO_MODE and SEED_ENABLED are both on
// (ADR-015 "Safe-by-default deployment"). Never call it from a non-demo path.
//
// Everything happens in one transaction, so the reset never exposes a
// half-purged database. keep must be non-empty (it always holds the three demo
// ids); an empty keep would match every tenant and is refused, as a guard
// against a caller wiping the entire tenants table by mistake.
func PurgeNonDemoTenants(ctx context.Context, pool *pgxpool.Pool, keep []string) (int64, error) {
	if len(keep) == 0 {
		return 0, fmt.Errorf("seed: PurgeNonDemoTenants: refusing to run with an empty keep set")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("seed: purge: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit

	// audit_log is append-only, guarded by a trigger that rejects DELETE. The
	// demo reset is the sanctioned exception; this transaction-local GUC lets the
	// purge clear it, exactly as seedTenant does for a single tenant.
	if _, err := tx.Exec(ctx, "SET LOCAL audit.allow_purge = 'on'"); err != nil {
		return 0, fmt.Errorf("seed: purge: enable audit purge: %w", err)
	}

	for _, table := range purgeOrder {
		if _, err := tx.Exec(ctx,
			"DELETE FROM "+table+" WHERE tenant_id <> ALL($1::uuid[])", keep); err != nil {
			return 0, fmt.Errorf("seed: purge: clear %s: %w", table, err)
		}
	}

	// fx_rates and fx_markup_defaults carry a nullable tenant_id: NULL rows are
	// global (env- or api-seeded) and must survive. Only delete rows owned by a
	// non-demo tenant.
	for _, table := range []string{"fx_rates", "fx_markup_defaults"} {
		if _, err := tx.Exec(ctx,
			"DELETE FROM "+table+" WHERE tenant_id IS NOT NULL AND tenant_id <> ALL($1::uuid[])", keep); err != nil {
			return 0, fmt.Errorf("seed: purge: clear %s: %w", table, err)
		}
	}

	tag, err := tx.Exec(ctx, "DELETE FROM tenants WHERE id <> ALL($1::uuid[])", keep)
	if err != nil {
		return 0, fmt.Errorf("seed: purge: clear tenants: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("seed: purge: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Demo resets all three demo tenants: the personal budget on
// defaultTenantID, plus a bank and a company on their own fixed ids. Each is
// reset and repopulated in its own transaction, so a failure on one is reported
// without half-seeding the others.
func Demo(ctx context.Context, pool *pgxpool.Pool, defaultTenantID string, now time.Time, currency, demoKeyHash string) error {
	if currency == "" {
		currency = "USD"
	}
	themes := []theme{
		personalTheme(defaultTenantID, currency),
		bankTheme(bankTenantID),
		companyTheme(companyTenantID),
	}
	for _, th := range themes {
		if err := seedTenant(ctx, pool, th, now, demoKeyHash); err != nil {
			return fmt.Errorf("seed demo tenant %q: %w", th.name, err)
		}
	}
	return nil
}

// seedTenant resets th.id and repopulates it from th's accounts and flows.
func seedTenant(ctx context.Context, pool *pgxpool.Pool, th theme, now time.Time, demoKeyHash string) error {
	tid, err := uuid.Parse(th.id)
	if err != nil {
		return fmt.Errorf("seed: parse tenant id: %w", err)
	}
	rng := rand.New(rand.NewSource(now.UnixNano() ^ int64(len(th.name)))) //nolint:gosec // demo data, not crypto
	accountsAt := now.AddDate(0, 0, -(historyDays + 1))

	// Resolve the chart of accounts (themed accounts plus the shared foreign
	// accounts) and assign each a fresh id keyed by name, so the flows can
	// reference accounts by name.
	accounts := make([]acct, 0, len(th.accounts)+len(foreignAccounts))
	accounts = append(accounts, th.accounts...)
	accounts = append(accounts, foreignAccounts...)
	idByName := make(map[string]string, len(accounts))
	curByName := make(map[string]string, len(accounts))
	for _, a := range accounts {
		idByName[a.name] = newID()
		cur := a.cur
		if cur == "" {
			cur = th.currency
		}
		curByName[a.name] = cur
	}

	// Build the transactions from the flows. Signed double-entry: positive
	// debit, negative credit; both legs in the theme currency.
	var txns []txn
	for _, f := range th.flows {
		for i := 0; i < f.count; i++ {
			c := f.cats[rng.Intn(len(f.cats))]
			amt := c.min + rng.Int63n(c.max-c.min+1)
			txns = append(txns, txn{randTime(rng, now), [2]posting{
				{idByName[f.debit], amt, c.desc},
				{idByName[f.credit], -amt, c.desc},
			}})
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("seed: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit

	// Ensure the tenant row exists before writing any tenant-owned data:
	// accounts_tenant_fk and transactions_tenant_fk (migration 0011, Task 2.1)
	// require it. This is a demo tenant, reset on a schedule, so its name is
	// stamped to a realistic entity name on every reset (it is not a real
	// customer whose name an operator would set); DO UPDATE SET name keeps it
	// looking like a real business in the console rather than a placeholder.
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`,
		tid, th.name); err != nil {
		return fmt.Errorf("seed: ensure tenant row: %w", err)
	}

	// Refuse to touch a tenant that holds any api key other than the demo key,
	// before any destructive statement runs. A misconfigured tenant id pointed
	// at a live tenant must never lose data to a periodic demo reset.
	var foreignKeys int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM api_keys WHERE tenant_id = $1 AND key_hash != $2",
		tid, demoKeyHash).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("seed: check api keys for tenant %s: %w", th.id, err)
	}
	if foreignKeys > 0 {
		return fmt.Errorf("seed: refusing to reset tenant %s: it holds %d api key(s) other than the demo key", th.id, foreignKeys)
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
	// pending_transactions (Task 12, ADR-025) holds this tenant's old seeded
	// pendings; clear it here too, before re-seeding a fresh batch below, so
	// a reset never leaves stale or duplicated pendings behind.
	for _, table := range []string{"idempotency_keys", "audit_log", "audit_outbox", "pending_transactions", "postings", "transactions", "accounts"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE tenant_id = $1", tid); err != nil {
			return fmt.Errorf("seed: clear %s: %w", table, err)
		}
	}

	// The public demo exposes the FX admin endpoints (internal/api/fx_admin.go)
	// with no auth in demo mode, so any anonymous visitor can POST a global
	// markup or a garbage global mid rate through them. Those rows are global
	// (tenant_id NULL), not scoped to a tenant, so the tenant-scoped loop above
	// never touches them and they would otherwise survive every reset and
	// mis-price every visitor's conversions. Clear only source='api' rows (the
	// admin-API write path, internal/fx/admin.go's apiSource): source='env'
	// rows are re-asserted at every boot from FX_RATES by fx.Seed, so they are
	// left alone. fx_rates is not in the tenant-scoped delete loop, so the
	// tenant's own api-sourced rows (an anonymous visitor can POST with a
	// tenant_id) are cleared here too, since CurrentFXRate prefers a
	// tenant-owned row over the global one.
	if _, err := tx.Exec(ctx, "DELETE FROM fx_rates WHERE tenant_id IS NULL AND source = 'api'"); err != nil {
		return fmt.Errorf("seed: clear api-sourced global fx rates: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM fx_rates WHERE tenant_id = $1 AND source = 'api'", tid); err != nil {
		return fmt.Errorf("seed: clear api-sourced tenant fx rates: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM fx_markup_defaults WHERE source = 'api' AND (tenant_id IS NULL OR tenant_id = $1)", tid); err != nil {
		return fmt.Errorf("seed: clear api-sourced fx markup defaults: %w", err)
	}

	// Prefill this tenant with starter FX rates and a 1 percent markup so the
	// Exchange rates page is not empty (demo only; see fx.PrefillDemoRates).
	if err := fx.PrefillDemoRates(ctx, fx.NewAdminService(tx), th.id); err != nil {
		return fmt.Errorf("seed: prefill demo fx rates: %w", err)
	}

	for _, a := range accounts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO accounts (id, tenant_id, name, type, currency, created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			idByName[a.name], tid, a.name, a.typ, curByName[a.name], accountsAt); err != nil {
			return fmt.Errorf("seed: insert account %s: %w", a.name, err)
		}
	}

	// transactions no longer carries a currency column (ADR-014, migration
	// 0010): currency lives on each posting instead. Every flow leg is in the
	// theme currency; the foreign accounts carry no flows, so no cross-currency
	// posting is seeded here.
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
				newID(), tid, txID, leg.accountID, leg.amount, leg.desc, th.currency, t.at); err != nil {
				return fmt.Errorf("seed: insert posting: %w", err)
			}
		}

		// Emit the same audit event a real post writes (ADR-017): a transaction.created
		// row in the outbox, which the background chainer later drains into the
		// tamper-evident audit_log. Without this, seeded transactions have no audit
		// trail and the audit view / chain is blank, an inconsistency with any
		// transaction posted through the API. occurred_at is the backdated posting
		// time, which the chainer copies into audit_log.created_at, so the audit
		// row lines up with the transaction it records. The after snapshot mirrors
		// the shape auditSnapshot produces in the service, so the console renders
		// the amount and account for seeded rows exactly as it does for live ones.
		after, err := auditAfter(txID, th.currency, t)
		if err != nil {
			return fmt.Errorf("seed: marshal audit snapshot: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_outbox (tenant_id, action, transaction_id, actor, after, occurred_at)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			tid, domain.ActionTransactionCreated, txID, tid.String(), after, t.at); err != nil {
			return fmt.Errorf("seed: insert audit outbox: %w", err)
		}
	}

	// Seed a few pending approvals (Task 12, ADR-025) so the demo Approvals
	// panel is never empty: a plausible large transfer between two of the
	// tenant's own accounts, held because it exceeds threshold_amt. This is
	// demo data only, written the same way the transactions above are:
	// nothing here calls ledger.ApprovalConfig.Gate, so these rows exist
	// regardless of whether APPROVAL_ENABLED is set on this deployment.
	// decided_by, decided_at, reason, and transaction_id are left NULL,
	// satisfying both pending_transactions CHECK constraints for a row still
	// in the 'pending' status.
	if len(th.accounts) >= 2 {
		debitID := idByName[th.accounts[0].name]
		creditID := idByName[th.accounts[1].name]

		// Scale the threshold to the tenant's own largest ordinary flow
		// amount, so "over threshold" reads as plausible across themes of
		// very different size: a bank's ledger runs two orders of magnitude
		// bigger than a personal budget's.
		var maxCat int64
		for _, f := range th.flows {
			for _, c := range f.cats {
				if c.max > maxCat {
					maxCat = c.max
				}
			}
		}
		if maxCat <= 0 {
			maxCat = 100000
		}

		pendingCount := 2 + rng.Intn(2) // 2 or 3
		for i := 0; i < pendingCount; i++ {
			// 25 to 75 percent over the threshold: clearly over, not a
			// borderline value.
			amt := maxCat + maxCat/4 + rng.Int63n(maxCat/2+1)
			desc := "Large transfer pending approval"
			payload, err := json.Marshal(map[string]any{
				"postings": []map[string]any{
					{"account_id": debitID, "amount": amt, "currency": th.currency, "description": desc},
					{"account_id": creditID, "amount": -amt, "currency": th.currency, "description": desc},
				},
			})
			if err != nil {
				return fmt.Errorf("seed: marshal pending payload: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO pending_transactions
				 (id, tenant_id, kind, payload, status, threshold_ccy, threshold_amt, created_by, created_at)
				 VALUES ($1,$2,'post',$3,'pending',$4,$5,$6,$7)`,
				newID(), tid, payload, th.currency, maxCat, tid.String(), randTime(rng, now)); err != nil {
				return fmt.Errorf("seed: insert pending transaction: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("seed: commit: %w", err)
	}
	return nil
}

// auditAfter builds the JSON after-snapshot for a seeded transaction, matching
// the shape internal/ledger.auditSnapshot writes for a live post: the id, a
// postings array of {account_id, amount, currency, description}, and the
// effective time. Amounts are minor units. Map keys marshal in sorted order, so
// the bytes are deterministic (the chainer hashes them verbatim).
func auditAfter(txID, currency string, t txn) ([]byte, error) {
	postings := make([]map[string]any, 0, len(t.legs))
	for _, leg := range t.legs {
		postings = append(postings, map[string]any{
			"account_id":  leg.accountID,
			"amount":      leg.amount,
			"currency":    currency,
			"description": leg.desc,
		})
	}
	return json.Marshal(map[string]any{
		"id":           txID,
		"postings":     postings,
		"effective_at": t.at,
	})
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
