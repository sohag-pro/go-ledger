package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sohag-pro/go-ledger/internal/domain"
	"github.com/sohag-pro/go-ledger/internal/postgres"
)

// seedTxn creates two accounts and posts one balanced transaction, returning the
// transaction id and the two account ids. It ensures tenant's own row exists
// first (accounts_tenant_fk, migration 0011): most callers pass a freshly
// generated id with no tenant row of its own, and a caller that already
// created one (e.g. to seed a second transaction for the same tenant) is
// unaffected, since CreateTenant is only ever a no-op the second time here
// (IsUniqueViolationError is swallowed).
func seedTxn(t *testing.T, repo *postgres.Repository, tenant string) (txnID, debit, credit string) { //nolint:unparam // credit is part of the helper's shape for future callers even though no test currently reads it
	t.Helper()
	ctx := context.Background()
	if err := repo.CreateTenant(ctx, tenant, "test tenant"); err != nil && !errors.Is(err, domain.ErrTenantAlreadyExists) {
		t.Fatalf("create tenant: %v", err)
	}
	d := &domain.Account{Name: "Cash", Type: domain.Asset, Currency: "USD"}
	c := &domain.Account{Name: "Revenue", Type: domain.Income, Currency: "USD"}
	if err := repo.CreateAccount(ctx, tenant, d); err != nil {
		t.Fatalf("create debit account: %v", err)
	}
	if err := repo.CreateAccount(ctx, tenant, c); err != nil {
		t.Fatalf("create credit account: %v", err)
	}
	txn := &domain.Transaction{Postings: []domain.Posting{
		{AccountID: d.ID, Amount: money(t, 500, "USD")},
		{AccountID: c.ID, Amount: money(t, -500, "USD")},
	}}
	if err := repo.CreateTransaction(ctx, tenant, txn); err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	return txn.ID, d.ID, c.ID
}

func TestIdempotencyKeyInsertAndDuplicate(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, tenant)

	// First insert of the key succeeds inside a Tx, with a generous ttl so it
	// stays live for the rest of this test.
	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "key-1", "fp-1", "v1", txnID, time.Hour)
	})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Second insert of the same key, while still live, returns
	// ErrDuplicateIdempotencyKey (Task 4.5, audit A1.4: the upsert only
	// replaces an EXPIRED row, so a live one still conflicts).
	err = repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "key-1", "fp-1", "v1", txnID, time.Hour)
	})
	if !errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		t.Fatalf("duplicate insert: got %v, want ErrDuplicateIdempotencyKey", err)
	}

	// The stored record round-trips, including the scheme it was written
	// under (Task 2.3, audit A1.6).
	rec, err := repo.GetIdempotencyKey(ctx, tenant, "key-1")
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if rec.Fingerprint != "fp-1" || rec.TransactionID != txnID || rec.Scheme != "v1" {
		t.Errorf("record = %+v, want fingerprint fp-1, scheme v1, and txn %s", rec, txnID)
	}

	// A missing key is a typed not-found.
	if _, err := repo.GetIdempotencyKey(ctx, tenant, "nope"); !errors.Is(err, domain.ErrIdempotencyKeyNotFound) {
		t.Errorf("missing key: got %v, want ErrIdempotencyKeyNotFound", err)
	}
}

// TestIdempotencyKeyExpiryTreatedAsAbsent proves an expired key behaves
// exactly like a key that was never written (Task 4.5, audit A1.4):
// GetIdempotencyKey returns ErrIdempotencyKeyNotFound, and a fresh insert
// under the same key succeeds rather than reporting a duplicate. ttl is a
// tiny real duration (not a backdated row) so this exercises the same
// "now() + ttl" server-side expiry stamping InsertIdempotencyKey does on
// every real call, not a fixture shortcut.
func TestIdempotencyKeyExpiryTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, tenant)

	const tinyTTL = 50 * time.Millisecond
	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "soon-expired", "fp-a", "v1", txnID, tinyTTL)
	})
	if err != nil {
		t.Fatalf("insert with tiny ttl: %v", err)
	}

	// Immediately after inserting, the key is still live.
	if _, err := repo.GetIdempotencyKey(ctx, tenant, "soon-expired"); err != nil {
		t.Fatalf("get before expiry: %v", err)
	}

	time.Sleep(2 * tinyTTL)

	// Past its ttl, the key reads back as absent.
	if _, err := repo.GetIdempotencyKey(ctx, tenant, "soon-expired"); !errors.Is(err, domain.ErrIdempotencyKeyNotFound) {
		t.Fatalf("get after expiry: got %v, want ErrIdempotencyKeyNotFound", err)
	}

	// A second transaction for the retry that would now proceed (the service
	// layer's precheck saw ErrIdempotencyKeyNotFound and posted for real).
	txnID2, _, _ := seedTxn(t, repo, tenant)

	// A new insert under the SAME key, pointing at a DIFFERENT transaction,
	// succeeds rather than conflicting: the stale row is replaced, not read
	// as a live duplicate.
	err = repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "soon-expired", "fp-b", "v1", txnID2, time.Hour)
	})
	if err != nil {
		t.Fatalf("re-insert after expiry: got %v, want nil (expired row should be replaced, not conflict)", err)
	}

	rec, err := repo.GetIdempotencyKey(ctx, tenant, "soon-expired")
	if err != nil {
		t.Fatalf("get after re-insert: %v", err)
	}
	if rec.Fingerprint != "fp-b" || rec.TransactionID != txnID2 {
		t.Errorf("record after replacing an expired key = %+v, want fingerprint fp-b and txn %s", rec, txnID2)
	}
}

// TestSweepExpiredIdempotencyKeys proves the background sweep (Task 4.5,
// audit A1.4) deletes only rows whose expiry has passed, leaving live rows
// untouched: the maintenance job that keeps idempotency_keys from growing
// forever must never delete a key a concurrent retry might still legitimately
// replay against.
func TestSweepExpiredIdempotencyKeys(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, _, _ := seedTxn(t, repo, tenant)

	// A live key: inserted with a generous ttl, must survive the sweep.
	if err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.InsertIdempotencyKey(ctx, tenant, "sweep-live", "fp-live", "v1", txnID, time.Hour)
	}); err != nil {
		t.Fatalf("insert live key: %v", err)
	}

	// Two expired keys: inserted live, then backdated directly (simulating
	// ttl having elapsed some time ago) so the sweep has deterministic rows
	// to find without a real sleep.
	for _, key := range []string{"sweep-expired-1", "sweep-expired-2"} {
		if err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
			return tx.InsertIdempotencyKey(ctx, tenant, key, "fp-expired", "v1", txnID, time.Hour)
		}); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE idempotency_keys SET expires_at = now() - interval '1 minute' WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenant, key,
		); err != nil {
			t.Fatalf("backdate %s: %v", key, err)
		}
	}

	// SweepExpiredIdempotencyKeys is deliberately NOT tenant-scoped (it is a
	// whole-table maintenance delete, see its doc comment), so under
	// t.Parallel() a sibling test's own short-lived expired rows may add to
	// the count this call reports: the deleted count is asserted as "at
	// least our two", not "exactly two", to keep this test honest about
	// what it can actually promise in a shared database. What IS
	// deterministic, and what the assertions below check, is that OUR two
	// rows are among the ones removed and OUR live row survives.
	deleted, err := repo.SweepExpiredIdempotencyKeys(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted < 2 {
		t.Errorf("sweep deleted %d rows, want at least 2 (our two expired keys)", deleted)
	}

	// The live key is untouched.
	if _, err := repo.GetIdempotencyKey(ctx, tenant, "sweep-live"); err != nil {
		t.Errorf("live key after sweep: %v", err)
	}

	// Our expired keys are gone even from a raw count, not just the
	// expiry-filtered lookup: the sweep physically deletes them.
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key IN ('sweep-expired-1', 'sweep-expired-2')`,
		tenant,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("remaining expired rows = %d, want 0", remaining)
	}

	// A second sweep runs cleanly (no error) even with nothing of ours left
	// to delete; it may still report a nonzero count from a sibling test's
	// own expired rows, so only the absence of an error is checked here.
	if _, err := repo.SweepExpiredIdempotencyKeys(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
}

func TestAuditAppendAndQuery(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	repo := postgres.NewRepository(pool)
	ctx := context.Background()
	tenant := uuid.NewString()
	txnID, debit, _ := seedTxn(t, repo, tenant)

	err := repo.RunInTx(ctx, tenant, func(ctx context.Context, tx domain.Tx) error {
		return tx.AppendAuditOutbox(ctx, tenant, domain.AuditEvent{
			Action:        domain.ActionTransactionCreated,
			TransactionID: txnID,
			Actor:         tenant,
			After:         []byte(`{"id":"` + txnID + `"}`),
		})
	})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}
	drainChainer(t, pool, tenant)

	byTxn, err := repo.ListAuditByTransaction(ctx, tenant, txnID)
	if err != nil {
		t.Fatalf("list by txn: %v", err)
	}
	if len(byTxn) != 1 || byTxn[0].Action != domain.ActionTransactionCreated {
		t.Fatalf("by txn = %+v, want one transaction.created row", byTxn)
	}
	if byTxn[0].ID == "" || byTxn[0].CreatedAt.IsZero() {
		t.Error("audit row missing generated id or created_at")
	}

	byAcct, err := repo.ListAuditByAccount(ctx, tenant, debit, nil, 50)
	if err != nil {
		t.Fatalf("list by account: %v", err)
	}
	if len(byAcct) != 1 || byAcct[0].TransactionID != txnID {
		t.Fatalf("by account = %+v, want the txn's audit row", byAcct)
	}
}
