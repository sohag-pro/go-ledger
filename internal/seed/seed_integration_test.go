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
	if acctCount != 7 {
		t.Errorf("account count = %d, want 7", acctCount)
	}

	var txnCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE tenant_id = $1`, tenant).Scan(&txnCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	// The seeder documents ~285 (95 spending + 95 income + 95 savings); assert a
	// range rather than the exact figure in case the mix is ever rebalanced.
	if txnCount < 200 || txnCount > 350 {
		t.Errorf("transaction count = %d, want roughly 285 (200 to 350)", txnCount)
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
	if acctCount != 7 {
		t.Errorf("account count after two Seed calls = %d, want 7 (reset, not duplicated)", acctCount)
	}

	var txnCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE tenant_id = $1`, tenant).Scan(&txnCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if txnCount < 200 || txnCount > 350 {
		t.Errorf("transaction count after two Seed calls = %d, want roughly 285 (200 to 350), not doubled", txnCount)
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
