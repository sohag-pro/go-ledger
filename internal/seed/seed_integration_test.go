package seed_test

// This file adds to the integration suite in seed_test.go, which already
// starts the shared Postgres container and defines TestMain, newTestPool,
// and the other scaffolding: reusing it here (same package, same directory)
// avoids a second, conflicting TestMain. What this file adds on top: a fixed,
// deterministic reference time (seed_test.go uses time.Now, which makes the
// random transaction mix vary run to run) and a strict per-transaction
// double-entry check, grouping postings by transaction id and asserting each
// group sums to zero, rather than only the global sum across accounts.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sohag-pro/go-ledger/internal/seed"
)

// TestSeed_PopulatesTenant checks the documented shape of demo data (4
// accounts, a few hundred backdated transactions) against a fixed reference
// time, and proves the double-entry invariant per transaction: since the
// seeder writes postings as raw rows rather than through the service layer,
// this is what actually guards it against ever writing unbalanced data.
func TestSeed_PopulatesTenant(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // fixed, deterministic

	if err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	var acctCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant).Scan(&acctCount); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if acctCount != 11 {
		t.Errorf("account count = %d, want 11", acctCount)
	}

	var txnCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE tenant_id = $1`, tenant).Scan(&txnCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	// The personal theme generates roughly 124 transactions; assert a range
	// rather than the exact figure in case the flow mix is ever rebalanced.
	if txnCount < 80 || txnCount > 200 {
		t.Errorf("transaction count = %d, want roughly 124 (80 to 200)", txnCount)
	}

	// The double-entry invariant, checked per transaction: every seeded
	// transaction's postings must sum to zero. Checking the global sum across
	// accounts (as TestSeed in seed_test.go does) cannot catch a pair of
	// offsetting mistakes in two different transactions; grouping by
	// transaction id here can.
	rows, err := pool.Query(ctx,
		`SELECT transaction_id, sum(amount) FROM postings WHERE tenant_id = $1 GROUP BY transaction_id`,
		tenant)
	if err != nil {
		t.Fatalf("sum postings: %v", err)
	}
	defer rows.Close()

	seenTxns := 0
	for rows.Next() {
		var txnID string
		var sum int64
		if err := rows.Scan(&txnID, &sum); err != nil {
			t.Fatalf("scan posting sum: %v", err)
		}
		seenTxns++
		if sum != 0 {
			t.Errorf("transaction %s postings sum to %d, want 0", txnID, sum)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate posting sums: %v", err)
	}
	if seenTxns != txnCount {
		t.Errorf("grouped %d transactions from postings, want %d (matching the transactions table)", seenTxns, txnCount)
	}

	// Every seeded transaction must emit exactly one transaction.created audit
	// outbox row (ADR-017), so the background chainer builds a tamper-evident
	// audit trail for seeded data just like it does for a live post. Without
	// this the audit view and chain are blank for prefilled data.
	var outboxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_outbox WHERE tenant_id = $1 AND action = 'transaction.created'`,
		tenant).Scan(&outboxCount); err != nil {
		t.Fatalf("count audit_outbox: %v", err)
	}
	if outboxCount != txnCount {
		t.Errorf("audit_outbox rows = %d, want %d (one per seeded transaction)", outboxCount, txnCount)
	}

	// The after snapshot must carry the postings (account_id + amount), the
	// fields the console renders, so seeded audit rows show amount and account.
	var withPostings int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_outbox
		 WHERE tenant_id = $1 AND json_array_length(after->'postings') = 2`,
		tenant).Scan(&withPostings); err != nil {
		t.Fatalf("check audit_outbox after snapshot: %v", err)
	}
	if withPostings != txnCount {
		t.Errorf("audit_outbox rows with a 2-leg postings snapshot = %d, want %d", withPostings, txnCount)
	}

	// Task 12 (ADR-025): the demo Approvals panel must never be empty. Seed a
	// few pending_transactions rows, all still pending, each carrying the
	// threshold it was held against, so a visitor sees something to approve
	// or reject on first load.
	var pendingCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_transactions WHERE tenant_id = $1 AND status = 'pending'`,
		tenant).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending_transactions: %v", err)
	}
	if pendingCount < 2 {
		t.Errorf("pending_transactions (status=pending) = %d, want at least 2", pendingCount)
	}

	var populatedThreshold int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_transactions
		 WHERE tenant_id = $1 AND status = 'pending'
		 AND threshold_ccy <> '' AND threshold_amt > 0`,
		tenant).Scan(&populatedThreshold); err != nil {
		t.Fatalf("count pending_transactions with a populated threshold: %v", err)
	}
	if populatedThreshold != pendingCount {
		t.Errorf("pending_transactions with populated threshold_ccy/threshold_amt = %d, want %d (all of them)",
			populatedThreshold, pendingCount)
	}
}

// TestSeed_ResetsRatherThanDuplicates calls Seed twice for the same tenant,
// with a fixed reference time, and checks the second call clears the first
// round's rows instead of piling on top of them: the account count must stay
// 4, not double to 8, matching the "resets the tenant's ledger" contract in
// Seed's doc comment.
func TestSeed_ResetsRatherThanDuplicates(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	tenant := uuid.NewString()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	if err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	var acctCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant).Scan(&acctCount); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if acctCount != 11 {
		t.Errorf("account count after two Seed calls = %d, want 11 (reset, not duplicated)", acctCount)
	}

	var txnCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE tenant_id = $1`, tenant).Scan(&txnCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if txnCount < 80 || txnCount > 200 {
		t.Errorf("transaction count after two Seed calls = %d, want roughly 124 (80 to 200), not doubled", txnCount)
	}

	// The reset must clear the first round's pending_transactions before
	// re-seeding, not pile a second batch on top: 2 to 3 pendings, same as a
	// single Seed call, never 4 to 6.
	var pendingCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_transactions WHERE tenant_id = $1`, tenant).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending_transactions: %v", err)
	}
	if pendingCount < 2 || pendingCount > 3 {
		t.Errorf("pending_transactions after two Seed calls = %d, want 2 to 3 (reset, not duplicated)", pendingCount)
	}
}

// insertAPIKey writes one api_keys row for tenant directly (the seeder itself
// never touches this table; only the app's provisioning path and this test
// setup do). It ensures tenant's own row exists first (api_keys_tenant_fk,
// migration 0011): the "only the demo key: proceeds" case below calls this
// before Seed ever runs for that tenant, so Seed's own tenant upsert has not
// happened yet.
func insertAPIKey(t *testing.T, pool *pgxpool.Pool, tenant, keyHash string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, 'test tenant') ON CONFLICT (id) DO NOTHING`,
		tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash) VALUES ($1, $2, $3, $4)`,
		uuid.NewString(), tenant, "test-key", keyHash); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
}

// TestPurgeNonDemoTenants proves the demo reset wipes visitor-created tenants
// wholesale: given a kept (demo) tenant and a victim tenant, both fully seeded,
// and the victim additionally holding its own api key, PurgeNonDemoTenants
// removes the victim and all its data across every tenant-scoped table while
// leaving the kept tenant untouched. Unlike Seed, the purge ignores the ADR-015
// api-key guard: a visitor tenant holding its own key is exactly what must be
// removed.
// Deliberately NOT parallel: PurgeNonDemoTenants deletes every tenant not in
// its keep set, so on this package's shared test database it would wipe the
// tenants other parallel tests create. Running it in the sequential phase (when
// every t.Parallel() test is paused before it has seeded anything) keeps it from
// racing them.
func TestPurgeNonDemoTenants(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	keep := uuid.NewString()
	victim := uuid.NewString()
	if err := seed.Seed(ctx, pool, keep, now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("seed kept tenant: %v", err)
	}
	if err := seed.Seed(ctx, pool, victim, now, "USD", testDemoKeyHash); err != nil {
		t.Fatalf("seed victim tenant: %v", err)
	}
	// The victim holds its own (non-demo) api key: the exact case Seed refuses to
	// touch but the purge must remove.
	insertAPIKey(t, pool, victim, "victim-key-hash")

	purged, err := seed.PurgeNonDemoTenants(ctx, pool, []string{keep})
	if err != nil {
		t.Fatalf("PurgeNonDemoTenants: %v", err)
	}
	if purged != 1 {
		t.Errorf("purged %d tenants, want 1", purged)
	}

	// The victim must be gone from tenants and every tenant-scoped table,
	// including pending_transactions (Task 12): Seed above already left the
	// victim holding 2 to 3 pendings, exactly the FK hazard purgeOrder must
	// clear before the final DELETE FROM tenants, since
	// pending_transactions.tenant_id references tenants(id).
	for _, table := range []string{"tenants", "accounts", "transactions", "postings", "audit_outbox", "api_keys", "pending_transactions"} {
		col := "tenant_id"
		if table == "tenants" {
			col = "id"
		}
		var n int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE "+col+" = $1", victim).Scan(&n); err != nil {
			t.Fatalf("count %s for victim: %v", table, err)
		}
		if n != 0 {
			t.Errorf("victim rows left in %s = %d, want 0", table, n)
		}
	}

	// The kept tenant must be fully intact.
	var keptAccts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE tenant_id = $1`, keep).Scan(&keptAccts); err != nil {
		t.Fatalf("count kept accounts: %v", err)
	}
	if keptAccts != 11 {
		t.Errorf("kept tenant accounts = %d, want 11 (untouched)", keptAccts)
	}

	// An empty keep set is refused, so a caller can never wipe every tenant.
	if _, err := seed.PurgeNonDemoTenants(ctx, pool, nil); err == nil {
		t.Error("PurgeNonDemoTenants with empty keep set: want error, got nil")
	}
}

// TestSeed_RefusesTenantWithForeignAPIKey proves the safe-by-default guard
// (ADR-015, "Safe-by-default deployment"): Seed must never wipe a tenant that
// holds an api key other than the demo key, since that is the signal that
// DEFAULT_TENANT_ID has been misconfigured to point at a real tenant instead
// of the demo one. Seed proceeds normally when the tenant has no api keys
// yet, or only the demo key, and refuses (wiping nothing) the moment any
// other key is present.
func TestSeed_RefusesTenantWithForeignAPIKey(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("no api keys yet: proceeds", func(t *testing.T) {
		tenant := uuid.NewString()
		if err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash); err != nil {
			t.Fatalf("Seed with no api keys: %v", err)
		}
	})

	t.Run("only the demo key: proceeds", func(t *testing.T) {
		tenant := uuid.NewString()
		insertAPIKey(t, pool, tenant, testDemoKeyHash)
		if err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash); err != nil {
			t.Fatalf("Seed with only the demo key present: %v", err)
		}
	})

	t.Run("a foreign key present: refuses and wipes nothing", func(t *testing.T) {
		tenant := uuid.NewString()
		// Seed once so the tenant has data worth protecting, then attach a
		// non-demo api key, simulating a real tenant's ledger.
		if err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash); err != nil {
			t.Fatalf("initial seed: %v", err)
		}
		insertAPIKey(t, pool, tenant, "some-other-tenants-key-hash")

		var acctBefore int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant).Scan(&acctBefore); err != nil {
			t.Fatalf("count accounts before refused reseed: %v", err)
		}

		err := seed.Seed(ctx, pool, tenant, now, "USD", testDemoKeyHash)
		if err == nil {
			t.Fatal("Seed against a tenant holding a foreign api key: got nil error, want a refusal")
		}

		var acctAfter int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant).Scan(&acctAfter); err != nil {
			t.Fatalf("count accounts after refused reseed: %v", err)
		}
		if acctAfter != acctBefore {
			t.Errorf("Seed wiped data despite refusing: accounts before=%d after=%d", acctBefore, acctAfter)
		}
	})
}
